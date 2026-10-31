package download

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/moov-io/base/log"
	"github.com/moov-io/base/telemetry"
	"github.com/moov-io/watchman/pkg/search"
)

// Trigger labels describe what initiated a refresh.
const (
	TriggerStartup   = "startup"
	TriggerScheduled = "scheduled"
	TriggerManual    = "manual"
)

// ErrAlreadyRunning is returned when a refresh is requested while another is in progress.
var ErrAlreadyRunning = errors.New("a data refresh is already running")

// State describes the lifecycle of the current or most recent refresh.
type State string

const (
	StateIdle      State = "idle"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	// StateDegraded means the refresh completed but at least one list named in
	// IgnoredDownloadErrors failed to load. Previously indexed data for those
	// lists was carried forward when available; see IgnoredErrors and
	// CarriedForwardLists on Status.
	StateDegraded State = "degraded"
)

// Status is a snapshot of the current or most recent data refresh.
type Status struct {
	State   State  `json:"state"`
	Trigger string `json:"trigger,omitempty"`

	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Duration   string     `json:"duration,omitempty"`
	LastError  string     `json:"lastError,omitempty"`

	// IgnoredErrors maps each list in IgnoredDownloadErrors that failed during
	// this refresh to its error. Non-empty IgnoredErrors sets State to degraded.
	IgnoredErrors map[string]string `json:"ignoredErrors,omitempty"`
	// CarriedForwardLists names the failed lists whose previously indexed
	// entities and hash were kept instead of being dropped from the index.
	CarriedForwardLists []string `json:"carriedForwardLists,omitempty"`
}

// statsUpdater receives download stats after a successful refresh (e.g. index.Lists)
// and exposes the currently indexed generation so a failed-but-ignored list can
// be carried forward instead of removed.
type statsUpdater interface {
	Update(Stats)
	LatestStats() Stats
	GetEntities(ctx context.Context, source search.SourceList) ([]search.Entity[search.Value], error)
}

// refreshResult summarizes the degraded-list outcome of one refresh.
type refreshResult struct {
	ignoredErrors  map[string]string
	carriedForward []string
}

// embeddingRebuilder rebuilds optional embedding indexes after refresh.
type embeddingRebuilder interface {
	RebuildEmbeddingIndex(ctx context.Context) error
}

// Refresher coordinates full data refreshes with single-flight semantics and status tracking.
type Refresher struct {
	baseCtx context.Context
	logger  log.Logger

	dl            Downloader
	indexedLists  statsUpdater
	searchService embeddingRebuilder

	mu      sync.Mutex
	running bool
	status  Status
}

func NewRefresher(baseCtx context.Context, logger log.Logger, dl Downloader, su statsUpdater, es embeddingRebuilder) *Refresher {
	return &Refresher{
		baseCtx:       baseCtx,
		logger:        logger,
		dl:            dl,
		indexedLists:  su,
		searchService: es,
		status:        Status{State: StateIdle},
	}
}

// RefreshNow runs a full refresh synchronously. Returns ErrAlreadyRunning if one is in progress.
func (r *Refresher) RefreshNow(ctx context.Context, trigger string) error {
	start, ok := r.start(trigger)
	if !ok {
		return ErrAlreadyRunning
	}
	res, err := r.run(ctx)
	r.finish(start, res, err)
	return err
}

// TriggerAsync starts a refresh in the background. Returns false if one is already running.
func (r *Refresher) TriggerAsync(trigger string) bool {
	start, ok := r.start(trigger)
	if !ok {
		return false
	}
	go func() {
		res, err := r.run(r.baseCtx)
		r.finish(start, res, err)
		if err != nil {
			r.logger.Error().LogErrorf("background %s data refresh failed: %v", trigger, err)
		}
	}()
	return true
}

// Status returns a snapshot of the current or most recent refresh.
func (r *Refresher) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (r *Refresher) start(trigger string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return time.Time{}, false
	}
	r.running = true
	now := time.Now().UTC()
	r.status = Status{
		State:     StateRunning,
		Trigger:   trigger,
		StartedAt: &now,
	}
	return now, true
}

func (r *Refresher) finish(start time.Time, res refreshResult, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	end := time.Now().UTC()
	r.running = false
	r.status.FinishedAt = &end
	r.status.Duration = end.Sub(start).String()
	r.status.IgnoredErrors = nil
	r.status.CarriedForwardLists = nil
	if err != nil {
		r.status.State = StateFailed
		r.status.LastError = err.Error()
		refreshDegraded.Set(0)
		return
	}
	r.status.LastError = ""
	if len(res.ignoredErrors) > 0 {
		r.status.State = StateDegraded
		r.status.IgnoredErrors = res.ignoredErrors
		r.status.CarriedForwardLists = res.carriedForward
		refreshDegraded.Set(1)
		return
	}
	r.status.State = StateSucceeded
	refreshDegraded.Set(0)
}

func (r *Refresher) run(ctx context.Context) (refreshResult, error) {
	ctx, span := telemetry.StartSpan(ctx, "refresh-all-sources")
	defer span.End()

	var res refreshResult

	stats, err := r.dl.RefreshAll(ctx)
	if err != nil {
		return res, err
	}

	if len(stats.IgnoredErrors) > 0 {
		res.ignoredErrors = stats.IgnoredErrors
		res.carriedForward = r.carryForwardIgnoredLists(ctx, &stats)
	}

	entityCount := len(stats.Entities)
	r.logger.Info().Logf("data refreshed - %v entities from %v lists took %v (using %.2fGB)",
		entityCount, len(stats.Lists), stats.EndedAt.Sub(stats.StartedAt), getCurrentMemoryUsed())

	if r.indexedLists != nil {
		// Index takes ownership of the entity slice inside the corpus. Clear our
		// local roots afterward so RefreshAll's return value does not keep the
		// previous generation + new slice alive on this stack until run returns.
		r.indexedLists.Update(stats)
		stats.Entities = nil
		stats.TFIDFIndex = nil
	}
	if r.searchService != nil {
		// Rebuild reads entities from the live index (corpus), not stats.Entities.
		if err := r.searchService.RebuildEmbeddingIndex(ctx); err != nil {
			return res, fmt.Errorf("failed to rebuild embedding index: %w", err)
		}
	}

	r.logger.Info().Logf("search index swapped (%d entities, heap %.2fGB)", entityCount, getCurrentMemoryUsed())
	return res, nil
}

// carryForwardIgnoredLists keeps the previously indexed generation of every list
// in stats.IgnoredErrors. Without this, index.Update would replace the whole
// corpus with a generation that lacks the failed list and screening against it
// (e.g. OFAC SDN) would silently stop. Returns the lists that were carried
// forward; lists that were never loaded stay absent and remain reported in
// IgnoredErrors.
func (r *Refresher) carryForwardIgnoredLists(ctx context.Context, stats *Stats) []string {
	failed := make([]string, 0, len(stats.IgnoredErrors))
	for name := range stats.IgnoredErrors {
		failed = append(failed, name)
	}
	slices.Sort(failed)

	for _, name := range failed {
		ignoredListErrors.WithLabelValues(name).Inc()
	}

	if r.indexedLists == nil {
		return nil
	}
	previous := r.indexedLists.LatestStats()

	var carried []string
	for _, name := range failed {
		if _, ok := previous.Lists[name]; !ok {
			r.logger.Warn().Logf("ignored list %s failed and has no previous data to keep", name)
			continue
		}

		entities, err := r.indexedLists.GetEntities(ctx, search.SourceList(name))
		if err != nil {
			r.logger.Error().LogErrorf("ignored list %s failed and previous data could not be read: %v", name, err)
			continue
		}

		stats.Lists[name] = len(entities)
		stats.ListHashes[name] = previous.ListHashes[name]
		stats.Entities = append(stats.Entities, entities...)
		carried = append(carried, name)

		r.logger.Warn().Logf("ignored list %s failed (%s) - keeping %d previously loaded entities",
			name, stats.IgnoredErrors[name], len(entities))
	}

	if len(carried) > 0 {
		// The TF-IDF index was built before the carried entities were appended.
		stats.TFIDFIndex = buildTFIDFIndex(r.logger, stats.Entities)
	}
	return carried
}

func getCurrentMemoryUsed() float64 {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	return float64(mem.Alloc) / 1024 / 1024 / 1024
}
