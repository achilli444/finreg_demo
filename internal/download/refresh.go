package download

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/moov-io/base/log"
	"github.com/moov-io/base/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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
)

// Status is a snapshot of the current or most recent data refresh.
type Status struct {
	State   State  `json:"state"`
	Trigger string `json:"trigger,omitempty"`

	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Duration   string     `json:"duration,omitempty"`
	LastError  string     `json:"lastError,omitempty"`

	// LastSuccessAt is when the currently served data was last refreshed successfully.
	// It survives later failures so operators can see how old the served lists are.
	LastSuccessAt       *time.Time `json:"lastSuccessAt,omitempty"`
	ConsecutiveFailures int        `json:"consecutiveFailures"`

	// Stale is true when LastSuccessAt is older than MaxStaleness (or no refresh has
	// ever succeeded). Stale data is still served; alert on this field or on the
	// watchman_data_stale metric.
	Stale        bool   `json:"stale"`
	MaxStaleness string `json:"maxStaleness,omitempty"`
}

var (
	metricLastSuccess = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "watchman", Subsystem: "data",
		Name: "refresh_last_success_timestamp_seconds",
		Help: "Unix time of the last successful sanctions data refresh (0 if never)",
	})
	metricRefreshFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchman", Subsystem: "data",
		Name: "refresh_failures_total",
		Help: "Number of failed sanctions data refreshes by trigger",
	}, []string{"trigger"})
	metricConsecutiveFailures = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "watchman", Subsystem: "data",
		Name: "refresh_consecutive_failures",
		Help: "Number of consecutive failed sanctions data refreshes",
	})
	metricStale = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "watchman", Subsystem: "data",
		Name: "stale",
		Help: "1 when the served sanctions data is older than Download.MaxStaleness, else 0",
	})
	metricListEntities = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "watchman", Subsystem: "data",
		Name: "list_entities",
		Help: "Entities indexed per list after the last successful refresh",
	}, []string{"list"})
)

// statsUpdater receives download stats after a successful refresh (e.g. index.Lists).
type statsUpdater interface {
	Update(Stats)
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

	maxStaleness time.Duration

	mu      sync.Mutex
	running bool
	status  Status
}

// SetMaxStaleness sets how old the last successful refresh may be before Status reports Stale.
func (r *Refresher) SetMaxStaleness(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maxStaleness = d
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
	err := r.run(ctx)
	r.finish(start, err)
	return err
}

// TriggerAsync starts a refresh in the background. Returns false if one is already running.
func (r *Refresher) TriggerAsync(trigger string) bool {
	start, ok := r.start(trigger)
	if !ok {
		return false
	}
	go func() {
		err := r.run(r.baseCtx)
		r.finish(start, err)
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
	st := r.status
	st.Stale = r.isStale(time.Now())
	if r.maxStaleness > 0 {
		st.MaxStaleness = r.maxStaleness.String()
	}
	return st
}

// IsStale reports whether the served data is older than the configured MaxStaleness.
func (r *Refresher) IsStale() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.isStale(time.Now())
}

func (r *Refresher) isStale(now time.Time) bool {
	if r.maxStaleness <= 0 {
		return false
	}
	if r.status.LastSuccessAt == nil {
		return true
	}
	return now.Sub(*r.status.LastSuccessAt) > r.maxStaleness
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
		State:               StateRunning,
		Trigger:             trigger,
		StartedAt:           &now,
		LastSuccessAt:       r.status.LastSuccessAt,
		ConsecutiveFailures: r.status.ConsecutiveFailures,
	}
	return now, true
}

func (r *Refresher) finish(start time.Time, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	end := time.Now().UTC()
	r.running = false
	r.status.FinishedAt = &end
	r.status.Duration = end.Sub(start).String()
	if err != nil {
		r.status.State = StateFailed
		r.status.LastError = err.Error()
		r.status.ConsecutiveFailures++
		metricRefreshFailures.WithLabelValues(r.status.Trigger).Inc()
		metricConsecutiveFailures.Set(float64(r.status.ConsecutiveFailures))
		metricStale.Set(boolToFloat(r.isStale(end)))
		return
	}
	r.status.State = StateSucceeded
	r.status.LastSuccessAt = &end
	r.status.ConsecutiveFailures = 0
	metricLastSuccess.Set(float64(end.Unix()))
	metricConsecutiveFailures.Set(0)
	metricStale.Set(0)
	r.status.LastError = ""
}

func (r *Refresher) run(ctx context.Context) error {
	ctx, span := telemetry.StartSpan(ctx, "refresh-all-sources")
	defer span.End()

	stats, err := r.dl.RefreshAll(ctx)
	if err != nil {
		return err
	}

	entityCount := len(stats.Entities)
	for list, count := range stats.Lists {
		metricListEntities.WithLabelValues(list).Set(float64(count))
	}
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
			return fmt.Errorf("failed to rebuild embedding index: %w", err)
		}
	}

	r.logger.Info().Logf("search index swapped (%d entities, heap %.2fGB)", entityCount, getCurrentMemoryUsed())
	return nil
}

func getCurrentMemoryUsed() float64 {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	return float64(mem.Alloc) / 1024 / 1024 / 1024
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
