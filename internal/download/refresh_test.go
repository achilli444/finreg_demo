package download

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/moov-io/base/log"
	"github.com/moov-io/watchman/pkg/search"

	"github.com/stretchr/testify/require"
)

// fakeDownloader implements Downloader for tests.
type fakeDownloader struct {
	stats   Stats
	err     error
	started chan struct{}
	release chan struct{}

	mu    sync.Mutex
	calls int
}

func (f *fakeDownloader) RefreshAll(ctx context.Context) (Stats, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.release != nil {
		<-f.release
	}
	if f.err != nil {
		return Stats{}, f.err
	}
	return f.stats, nil
}

func (f *fakeDownloader) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func okStats() Stats {
	return Stats{
		Lists:      map[string]int{"us_ofac": 3},
		ListHashes: map[string]string{"us_ofac": "abc"},
	}
}

// testStatsUpdater is a minimal statsUpdater for tests that records the last update
// and serves it back as the "indexed" generation.
type testStatsUpdater struct {
	mu   sync.Mutex
	last Stats
}

func (t *testStatsUpdater) Update(s Stats) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last = s
}

func (t *testStatsUpdater) LatestStats() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.last
}

func (t *testStatsUpdater) GetEntities(_ context.Context, source search.SourceList) ([]search.Entity[search.Value], error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []search.Entity[search.Value]
	for _, e := range t.last.Entities {
		if e.Source == source {
			out = append(out, e)
		}
	}
	return out, nil
}

func (t *testStatsUpdater) latest() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.last
}

func newTestRefresher(t *testing.T, dl Downloader) (*Refresher, *testStatsUpdater) {
	t.Helper()
	up := &testStatsUpdater{}
	r := NewRefresher(context.Background(), log.NewTestLogger(), dl, up, nil)
	return r, up
}

func TestRefresher_RefreshNowSuccess(t *testing.T) {
	dl := &fakeDownloader{stats: okStats()}
	r, up := newTestRefresher(t, dl)

	require.Equal(t, StateIdle, r.Status().State)

	err := r.RefreshNow(context.Background(), TriggerStartup)
	require.NoError(t, err)

	st := r.Status()
	require.Equal(t, StateSucceeded, st.State)
	require.Equal(t, TriggerStartup, st.Trigger)
	require.NotNil(t, st.StartedAt)
	require.NotNil(t, st.FinishedAt)
	require.NotEmpty(t, st.Duration)
	require.Empty(t, st.LastError)

	require.Equal(t, map[string]int{"us_ofac": 3}, up.latest().Lists)
}

func ofacEntities(n int) []search.Entity[search.Value] {
	out := make([]search.Entity[search.Value], 0, n)
	for i := 0; i < n; i++ {
		out = append(out, search.Entity[search.Value]{
			Name:     fmt.Sprintf("SDN %d", i),
			Type:     search.EntityPerson,
			Source:   search.SourceUSOFAC,
			SourceID: fmt.Sprintf("%d", i),
		}.Normalize())
	}
	return out
}

func euEntity() search.Entity[search.Value] {
	return search.Entity[search.Value]{
		Name: "EU Person", Type: search.EntityPerson, Source: search.SourceEUCSL, SourceID: "eu1",
	}.Normalize()
}

// TestRefresher_IgnoredListCarriedForward verifies that when a list named in
// IgnoredDownloadErrors fails, its previously indexed entities and hash are kept
// instead of the whole list vanishing from the corpus, and that the refresh is
// reported as degraded rather than succeeded.
func TestRefresher_IgnoredListCarriedForward(t *testing.T) {
	dl := &fakeDownloader{stats: Stats{
		Entities:   append(ofacEntities(3), euEntity()),
		Lists:      map[string]int{"us_ofac": 3, "eu_csl": 1},
		ListHashes: map[string]string{"us_ofac": "abc", "eu_csl": "eu"},
	}}
	r, up := newTestRefresher(t, dl)
	require.NoError(t, r.RefreshNow(context.Background(), TriggerStartup))
	require.Equal(t, StateSucceeded, r.Status().State)

	// Second refresh: OFAC fetch fails but is ignored, so only EU CSL comes back.
	dl.stats = Stats{
		Entities:      []search.Entity[search.Value]{euEntity()},
		Lists:         map[string]int{"eu_csl": 1},
		ListHashes:    map[string]string{"eu_csl": "eu2"},
		IgnoredErrors: map[string]string{"us_ofac": "download failed: 503"},
	}
	require.NoError(t, r.RefreshNow(context.Background(), TriggerScheduled))

	st := r.Status()
	require.Equal(t, StateDegraded, st.State)
	require.Empty(t, st.LastError)
	require.Equal(t, map[string]string{"us_ofac": "download failed: 503"}, st.IgnoredErrors)
	require.Equal(t, []string{"us_ofac"}, st.CarriedForwardLists)

	latest := up.latest()
	require.Equal(t, map[string]int{"us_ofac": 3, "eu_csl": 1}, latest.Lists)
	require.Equal(t, "abc", latest.ListHashes["us_ofac"], "previous OFAC hash must be kept")
	require.Equal(t, "eu2", latest.ListHashes["eu_csl"])
	require.Len(t, latest.Entities, 4)
	require.NotNil(t, latest.TFIDFIndex, "TF-IDF index must be rebuilt over carried entities")

	var ofac int
	for _, e := range latest.Entities {
		if e.Source == search.SourceUSOFAC {
			ofac++
		}
	}
	require.Equal(t, 3, ofac, "OFAC entities must survive an ignored fetch failure")

	// A later healthy refresh clears the degraded state.
	dl.stats = okStats()
	require.NoError(t, r.RefreshNow(context.Background(), TriggerScheduled))
	st = r.Status()
	require.Equal(t, StateSucceeded, st.State)
	require.Empty(t, st.IgnoredErrors)
	require.Empty(t, st.CarriedForwardLists)
}

// TestRefresher_IgnoredListNoPreviousData verifies that an ignored failure on a
// list that was never loaded is still surfaced as degraded, with nothing to
// carry forward.
func TestRefresher_IgnoredListNoPreviousData(t *testing.T) {
	dl := &fakeDownloader{stats: Stats{
		Lists:         map[string]int{},
		ListHashes:    map[string]string{},
		IgnoredErrors: map[string]string{"us_ofac": "boom"},
	}}
	r, up := newTestRefresher(t, dl)

	require.NoError(t, r.RefreshNow(context.Background(), TriggerStartup))

	st := r.Status()
	require.Equal(t, StateDegraded, st.State)
	require.Equal(t, "boom", st.IgnoredErrors["us_ofac"])
	require.Empty(t, st.CarriedForwardLists)
	require.NotContains(t, up.latest().Lists, "us_ofac")
}

func TestRefresher_RefreshNowFailure(t *testing.T) {
	dl := &fakeDownloader{err: errors.New("boom")}
	r, up := newTestRefresher(t, dl)

	err := r.RefreshNow(context.Background(), TriggerManual)
	require.Error(t, err)

	st := r.Status()
	require.Equal(t, StateFailed, st.State)
	require.Equal(t, "boom", st.LastError)
	require.NotNil(t, st.FinishedAt)

	require.Empty(t, up.latest().Lists)

	// Slot must be released
	dl.err = nil
	require.NoError(t, r.RefreshNow(context.Background(), TriggerScheduled))
	require.Equal(t, StateSucceeded, r.Status().State)
}

func TestRefresher_ConcurrencyGuard(t *testing.T) {
	dl := &fakeDownloader{
		stats:   okStats(),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	r, _ := newTestRefresher(t, dl)

	done := make(chan error, 1)
	go func() {
		done <- r.RefreshNow(context.Background(), TriggerScheduled)
	}()

	<-dl.started

	require.Equal(t, StateRunning, r.Status().State)

	require.ErrorIs(t, r.RefreshNow(context.Background(), TriggerManual), ErrAlreadyRunning)
	require.False(t, r.TriggerAsync(TriggerManual))

	close(dl.release)
	require.NoError(t, <-done)

	require.Equal(t, StateSucceeded, r.Status().State)
	require.Equal(t, 1, dl.callCount())
}

func TestRefresher_TriggerAsync(t *testing.T) {
	dl := &fakeDownloader{stats: okStats()}
	r, _ := newTestRefresher(t, dl)

	require.True(t, r.TriggerAsync(TriggerManual))

	require.Eventually(t, func() bool {
		return r.Status().State == StateSucceeded
	}, 2*time.Second, 5*time.Millisecond)

	require.Equal(t, TriggerManual, r.Status().Trigger)
}
