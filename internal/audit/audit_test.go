package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moov-io/base/log"
	"github.com/moov-io/watchman/internal/download"
	internalsearch "github.com/moov-io/watchman/internal/search"
	"github.com/moov-io/watchman/pkg/search"

	"github.com/stretchr/testify/require"
)

type fakeSearch struct {
	results []search.SearchedEntity[search.Value]
	err     error
}

func (f *fakeSearch) LatestStats() download.Stats {
	return download.Stats{
		Lists:      map[string]int{"us_ofac": 2},
		ListHashes: map[string]string{"us_ofac": "deadbeef"},
		StartedAt:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		EndedAt:    time.Date(2026, 1, 2, 3, 5, 5, 0, time.UTC),
		Version:    "test",
	}
}

func (f *fakeSearch) Search(ctx context.Context, q search.Entity[search.Value], opts internalsearch.SearchOpts) ([]search.SearchedEntity[search.Value], error) {
	return f.results, f.err
}

func (f *fakeSearch) RebuildEmbeddingIndex(ctx context.Context) error { return nil }

func testQuery() search.Entity[search.Value] {
	return search.Entity[search.Value]{
		Name:   "Nicolas Maduro",
		Type:   search.EntityPerson,
		Source: search.SourceAPIRequest,
	}
}

func testHit() search.SearchedEntity[search.Value] {
	return search.SearchedEntity[search.Value]{
		Entity: search.Entity[search.Value]{
			Name:     "MADURO MOROS, Nicolas",
			Type:     search.EntityPerson,
			Source:   search.SourceUSOFAC,
			SourceID: "22790",
		},
		Match: 0.93,
		Details: search.SimilarityScore{
			FinalScore: 0.93,
			Pieces:     []search.ScorePiece{{PieceType: "name", Score: 0.93, Weight: 35, Matched: true, FieldsCompared: 1}},
		},
	}
}

func readEvents(t *testing.T, dir string) []Event {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, filePrefix+"*.jsonl"))
	require.NoError(t, err)
	var out []Event
	for _, m := range matches {
		fd, err := os.Open(m)
		require.NoError(t, err)
		sc := bufio.NewScanner(fd)
		for sc.Scan() {
			var ev Event
			require.NoError(t, json.Unmarshal(sc.Bytes(), &ev))
			out = append(out, ev)
		}
		fd.Close()
	}
	return out
}

func TestService_RecordsSuccessfulSearch(t *testing.T) {
	dir := t.TempDir()
	logger := log.NewTestLogger()
	cfg := Config{Enabled: true, Directory: dir, IncludeQuery: true, IncludeDetails: true}
	sink, err := NewSink(logger, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { sink.Close() })

	inner := &fakeSearch{results: []search.SearchedEntity[search.Value]{testHit()}}
	svc := Wrap(logger, cfg, sink, inner)

	results, err := svc.Search(context.Background(), testQuery(), internalsearch.SearchOpts{
		Limit: 10, MinMatch: 0.80, RequestID: "req-1",
	})
	require.NoError(t, err)
	require.Len(t, results, 1)

	events := readEvents(t, dir)
	require.Len(t, events, 1)
	ev := events[0]

	require.Equal(t, "req-1", ev.RequestID)
	require.Equal(t, ChannelAPI, ev.Channel)
	require.Equal(t, "jaro-winkler", ev.Algorithm)
	require.InDelta(t, 0.80, ev.MinMatch, 0.0001)
	require.Equal(t, 10, ev.Limit)
	require.NotNil(t, ev.Query)
	require.Equal(t, "Nicolas Maduro", ev.Query.Name)
	require.Len(t, ev.QueryHash, 64)

	require.NotNil(t, ev.ListInfo)
	require.Equal(t, "deadbeef", ev.ListInfo.ListHashes["us_ofac"])
	require.Equal(t, 2, ev.ListInfo.Lists["us_ofac"])
	require.Equal(t, "test", ev.ListInfo.Version)

	require.Equal(t, 1, ev.ResultCount)
	require.InDelta(t, 0.93, ev.TopMatch, 0.0001)
	require.Len(t, ev.Results, 1)
	require.Equal(t, "us_ofac", ev.Results[0].SourceList)
	require.Equal(t, "22790", ev.Results[0].SourceID)
	require.NotNil(t, ev.Results[0].Details)
	require.Len(t, ev.Results[0].Details.Pieces, 1)
	require.Empty(t, ev.Error)
}

func TestService_RedactsQueryAndRecordsErrors(t *testing.T) {
	dir := t.TempDir()
	logger := log.NewTestLogger()
	cfg := Config{Enabled: true, Directory: dir, IncludeQuery: false}
	sink, err := NewSink(logger, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { sink.Close() })

	inner := &fakeSearch{err: errors.New("index unavailable")}
	svc := Wrap(logger, cfg, sink, inner)

	q := testQuery()
	q.Source = search.SourceMCPRequest
	_, err = svc.Search(context.Background(), q, internalsearch.SearchOpts{Limit: 5})
	require.Error(t, err)

	events := readEvents(t, dir)
	require.Len(t, events, 1)
	require.Nil(t, events[0].Query, "query PII must not be written when IncludeQuery=false")
	require.Len(t, events[0].QueryHash, 64)
	require.Equal(t, ChannelMCP, events[0].Channel)
	require.Equal(t, "index unavailable", events[0].Error)
	require.Equal(t, 0, events[0].ResultCount)
}

func TestFileSink_RetentionPrunesOldFiles(t *testing.T) {
	dir := t.TempDir()
	logger := log.NewTestLogger()

	old := filepath.Join(dir, filePrefix+"2020-01-01.jsonl")
	require.NoError(t, os.WriteFile(old, []byte("{}\n"), 0o640))
	unrelated := filepath.Join(dir, "keep-me.txt")
	require.NoError(t, os.WriteFile(unrelated, []byte("x"), 0o640))

	sink, err := newFileSink(logger, dir, 30)
	require.NoError(t, err)
	t.Cleanup(func() { sink.Close() })

	require.NoError(t, sink.Write(Event{Timestamp: time.Now().UTC()}))

	_, err = os.Stat(old)
	require.True(t, os.IsNotExist(err), "file older than retention should be removed")
	_, err = os.Stat(unrelated)
	require.NoError(t, err, "files the sink did not create must never be removed")

	today := filepath.Join(dir, filePrefix+time.Now().UTC().Format("2006-01-02")+".jsonl")
	_, err = os.Stat(today)
	require.NoError(t, err)
}

func TestFileSink_NoRetentionKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, filePrefix+"2020-01-01.jsonl")
	require.NoError(t, os.WriteFile(old, []byte("{}\n"), 0o640))

	sink, err := newFileSink(log.NewTestLogger(), dir, 0)
	require.NoError(t, err)
	t.Cleanup(func() { sink.Close() })
	require.NoError(t, sink.Write(Event{Timestamp: time.Now().UTC()}))

	_, err = os.Stat(old)
	require.NoError(t, err)
}
