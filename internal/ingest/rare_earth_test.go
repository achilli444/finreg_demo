package ingest_test

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/moov-io/base/log"
	"github.com/moov-io/watchman/internal/config"
	"github.com/moov-io/watchman/internal/index"
	"github.com/moov-io/watchman/internal/ingest"
	"github.com/moov-io/watchman/internal/search"
	pubsearch "github.com/moov-io/watchman/pkg/search"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

// The rare-earth-business profile ships in configs/config.default.yml, so this
// test loads the defaults (no override files) and uploads the seed CSV.
func TestIngest_RareEarthBusiness(t *testing.T) {
	t.Setenv("APP_CONFIG", "")
	t.Setenv("APP_CONFIG_SECRETS", "")

	logger := log.NewTestLogger()
	ctx := context.Background()

	conf, err := config.LoadConfig(logger)
	require.NoError(t, err)

	file, ok := conf.Ingest.Files["rare-earth-business"]
	require.True(t, ok, "rare-earth-business fileType missing from configs/config.default.yml")
	require.Equal(t, ingest.FormatCSV, file.Format)
	require.Equal(t, "business", file.Mapping.Type.Default)
	require.Equal(t, "record_id", file.Mapping.SourceID.Column)

	repo := &ingest.MockRepository{}
	indexedLists := index.NewLists(repo)

	searchService, err := search.NewService(logger, search.DefaultConfig(), nil, indexedLists)
	require.NoError(t, err)

	ingestService := ingest.NewService(logger, conf.Ingest, repo)
	controller := ingest.NewController(logger, ingestService, conf.Ingest, indexedLists)

	router := mux.NewRouter()
	controller.AppendRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	fd, err := os.Open(filepath.Join("..", "..", "configs", "ingest", "rare-earth-entities.csv"))
	require.NoError(t, err)
	t.Cleanup(func() { fd.Close() })

	client := pubsearch.NewClient(nil, server.URL)
	resp, err := client.IngestFile(ctx, "rare-earth-business", fd)
	require.NoError(t, err)
	require.Equal(t, "rare-earth-business", resp.FileType)

	// 14 rows, two of which repeat a record_id to add an alias
	require.Len(t, resp.Entities, 12)

	var baiyun *pubsearch.Entity[pubsearch.Value]
	for i := range resp.Entities {
		require.Equal(t, pubsearch.EntityBusiness, resp.Entities[i].Type)
		require.NotEmpty(t, resp.Entities[i].SourceID)
		if resp.Entities[i].SourceID == "RE-2026-0003" {
			baiyun = &resp.Entities[i]
		}
	}
	require.NotNil(t, baiyun)
	require.Equal(t, "Baiyun Neodymium Sourcing Ltd.", baiyun.Name)
	require.NotNil(t, baiyun.Business)
	// the alias column plus the repeated record_id row (Merge also keeps the primary name as an alias)
	require.Subset(t, baiyun.Business.AltNames, []string{"Baiyun Nd Sourcing", "Baiyun Neodymium Supply Chain Co."})
	// the CSV reader keeps the identifier only; id_type is informational
	require.ElementsMatch(t, []pubsearch.GovernmentID{
		{Identifier: "91150200MA3DK9QX0F"},
	}, baiyun.Business.GovernmentIDs)
	require.Len(t, baiyun.Addresses, 1)
	require.Equal(t, "Baotou", baiyun.Addresses[0].City)
	require.Equal(t, "CN", baiyun.Addresses[0].Country)

	// GET /v2/listinfo shows the source next to downloaded lists
	stats := searchService.LatestStats()
	require.Equal(t, 12, stats.Lists["rare-earth-business"])
	require.NotEmpty(t, stats.ListHashes["rare-earth-business"])

	// Screening: source is the fileType, type stays business
	query := pubsearch.Entity[pubsearch.Value]{
		Name:   "Meridian Lanthanide Trading",
		Type:   pubsearch.EntityBusiness,
		Source: pubsearch.SourceList("rare-earth-business"),
	}.Normalize()

	results, err := searchService.Search(ctx, query, search.SearchOpts{Limit: 1, MinMatch: 0.80})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "Meridian Lanthanide Trading Co.", results[0].Name)
	require.Equal(t, "RE-2026-0001", results[0].SourceID)
	require.GreaterOrEqual(t, results[0].Match, 0.80)

	// Alias rows are searchable too
	query.Name = "Ganzhou HREE Consolidators"
	query = query.Normalize()
	results, err = searchService.Search(ctx, query, search.SearchOpts{Limit: 1, MinMatch: 0.80})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "RE-2026-0008", results[0].SourceID)

	// Wrong type only searches that partition and finds nothing
	query.Type = pubsearch.EntityPerson
	results, err = searchService.Search(ctx, query, search.SearchOpts{Limit: 1, MinMatch: 0.80})
	require.NoError(t, err)
	require.Empty(t, results)
}
