package ingest_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moov-io/base/log"
	"github.com/moov-io/watchman/internal/compress"
	"github.com/moov-io/watchman/internal/config"
	"github.com/moov-io/watchman/internal/db"
	"github.com/moov-io/watchman/internal/index"
	"github.com/moov-io/watchman/internal/ingest"
	"github.com/moov-io/watchman/internal/search"
	pubsearch "github.com/moov-io/watchman/pkg/search"
	"github.com/moov-io/watchman/pkg/sources/senzing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

func TestIngest_API(t *testing.T) {
	setupIngestAPITest(t, func(scope ingestApiSetup) {
		file, err := os.Open(filepath.Join("testdata", "fincen-person.csv"))
		require.NoError(t, err)
		t.Cleanup(func() { file.Close() })

		ctx := context.Background()
		ingestResponse, err := scope.client.IngestFile(ctx, "fincen-person", file)
		require.NoError(t, err)

		require.Equal(t, "fincen-person", ingestResponse.FileType)
		require.Len(t, ingestResponse.Entities, 3)

		// Perform a search against the ingested file
		query := pubsearch.Entity[pubsearch.Value]{
			Name:   "John K Doe1",
			Type:   pubsearch.EntityPerson,
			Source: pubsearch.SourceList("fincen-person"),
			Addresses: []pubsearch.Address{
				{
					Line1:      "193 Southfield Lane",
					City:       "Anytown",
					PostalCode: "90210",
					State:      "CA",
					Country:    "US",
				},
			},
		}
		searchResponse, err := scope.searchService.Search(ctx, query.Normalize(), search.SearchOpts{
			Limit: 1,
			Debug: true,
		})
		require.NoError(t, err)
		require.Len(t, searchResponse, 1)

		// Sanity check the response
		require.Equal(t, "John Jr K Doe1", searchResponse[0].Name)
		require.Equal(t, pubsearch.SourceList("fincen-person"), searchResponse[0].Source)
		require.InDelta(t, searchResponse[0].Match, 0.839, 0.001)
	})
}

func TestIngest_API_Gzip(t *testing.T) {
	setupIngestAPITest(t, func(scope ingestApiSetup) {
		// Replace linux line endings with windows (\r\n)
		bs, err := os.ReadFile(filepath.Join("testdata", "Person12.09.2025.csv"))
		require.NoError(t, err)

		lines := bytes.Split(bs, []byte("\n"))
		require.Len(t, lines, 4)

		var file bytes.Buffer
		for i := range lines {
			file.Write(lines[i])
			file.WriteString("\r\n")
		}

		// Make ingest API request
		ctx := context.Background()
		body := compress.GzipTestFile(t, &file)

		ingestResponse, err := scope.client.IngestFile(ctx, "fincen-person", body)
		require.NoError(t, err)

		require.Equal(t, "fincen-person", ingestResponse.FileType)
		require.Len(t, ingestResponse.Entities, 2)

		// Perform a search against the ingested file
		query := pubsearch.Entity[pubsearch.Value]{
			Name:   "John K Doe1",
			Type:   pubsearch.EntityPerson,
			Source: pubsearch.SourceList("fincen-person"),
			Addresses: []pubsearch.Address{
				{
					Line1:      "193 Southfield Lane",
					City:       "Anytown",
					PostalCode: "90210",
					State:      "CA",
					Country:    "US",
				},
			},
		}
		searchResponse, err := scope.searchService.Search(ctx, query.Normalize(), search.SearchOpts{
			Limit: 1,
			Debug: true,
		})
		require.NoError(t, err)
		require.Len(t, searchResponse, 1)

		// Sanity check the response
		require.Equal(t, "John Doe", searchResponse[0].Name)
		require.Equal(t, pubsearch.SourceList("fincen-person"), searchResponse[0].Source)
		require.InDelta(t, searchResponse[0].Match, 0.8198, 0.001)
	})
}

func TestAPI_Senzing(t *testing.T) {
	setupIngestAPITest(t, func(scope ingestApiSetup) {
		file, err := os.Open(filepath.Join("testdata", "fincen-person.csv"))
		require.NoError(t, err)
		t.Cleanup(func() { file.Close() })

		ctx := context.Background()
		ingestResponse, err := scope.client.IngestFile(ctx, "fincen-person", file)
		require.NoError(t, err)

		require.Equal(t, "fincen-person", ingestResponse.FileType)
		require.Len(t, ingestResponse.Entities, 3)

		// Export the data
		req, err := http.NewRequest("GET", scope.server.URL+"/v2/export/fincen-person", nil)
		require.NoError(t, err)

		req.Header.Set("Accept", "senzing/jsonl")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		if resp != nil && resp.Body != nil {
			defer resp.Body.Close()
		}

		bs, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		// Make sure the response is JSON Lines
		require.True(t, bytes.HasPrefix(bs, []byte("{")))

		entities, err := senzing.ReadEntities(bytes.NewReader(bs), pubsearch.SourceList("senzing"))
		require.NoError(t, err)
		require.Len(t, entities, 3)
	})
}

type ingestApiSetup struct {
	client pubsearch.Client
	server *httptest.Server

	searchService search.Service
}

func setupIngestAPITest(t *testing.T, fn func(ingestApiSetup)) {
	t.Helper()

	db.ForEachDatabase(t, func(db db.DB) {
		logger := log.NewTestLogger()

		ingestRepository := ingest.NewRepository(db)
		indexedLists := index.NewLists(ingestRepository)

		searchConfig := search.DefaultConfig()
		searchService, err := search.NewService(logger, searchConfig, db, indexedLists)
		require.NoError(t, err)

		t.Setenv("APP_CONFIG_SECRETS", filepath.Join("testdata", "fincen-config.yml"))
		ingestConf, err := config.LoadConfig(logger)
		require.NoError(t, err)

		ingestService := ingest.NewService(logger, ingestConf.Ingest, ingestRepository)
		controller := ingest.NewController(logger, ingestService, ingestConf.Ingest, indexedLists)

		router := mux.NewRouter()
		controller.AppendRoutes(router)

		server := httptest.NewServer(router)
		t.Cleanup(func() { server.Close() })

		// Setup our client
		client := pubsearch.NewClient(nil, server.URL)

		fn(ingestApiSetup{
			client:        client,
			server:        server,
			searchService: searchService,
		})
	})
}

type recordingIngestService struct {
	reads int
}

func (s *recordingIngestService) ReadEntitiesFromFile(_ context.Context, name string, contents io.Reader) (ingest.FileEntities, error) {
	s.reads++
	if contents != nil {
		if _, err := io.Copy(io.Discard, contents); err != nil {
			return ingest.FileEntities{}, err
		}
	}
	return ingest.FileEntities{
		FileType: name,
		Entities: []pubsearch.Entity[pubsearch.Value]{{Name: "ok"}},
	}, nil
}

func (s *recordingIngestService) ReplaceEntities(context.Context, string, []pubsearch.Entity[pubsearch.Value]) error {
	return nil
}

func (s *recordingIngestService) GetEntitiesBySource(context.Context, string) ([]pubsearch.Entity[pubsearch.Value], error) {
	return nil, nil
}

func TestIngestFile_MaxBodyBytes(t *testing.T) {
	logger := log.NewTestLogger()
	svc := &recordingIngestService{}
	controller := ingest.NewController(logger, svc, ingest.Config{MaxBodyBytes: 64}, nil)

	router := mux.NewRouter()
	controller.AppendRoutes(router)

	t.Run("under limit", func(t *testing.T) {
		svc.reads = 0
		req := httptest.NewRequest("POST", "/v2/ingest/custom", bytes.NewReader([]byte("tiny body")))
		req.Header.Set("Content-Type", "text/csv")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, 1, svc.reads)
	})

	t.Run("content-length over limit", func(t *testing.T) {
		svc.reads = 0
		body := bytes.Repeat([]byte("a"), 100)
		req := httptest.NewRequest("POST", "/v2/ingest/custom", bytes.NewReader(body))
		req.Header.Set("Content-Type", "text/csv")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		require.Contains(t, w.Body.String(), "request body exceeds 64 byte limit")
		require.Equal(t, 0, svc.reads)
	})

	t.Run("chunked body over limit", func(t *testing.T) {
		svc.reads = 0
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write(bytes.Repeat([]byte("x"), 200))
			_ = pw.Close()
		}()

		req := httptest.NewRequest("POST", "/v2/ingest/custom", pr)
		req.Header.Set("Content-Type", "text/csv")
		req.ContentLength = -1
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		require.Contains(t, w.Body.String(), "request body exceeds 64 byte limit")
		require.Equal(t, 1, svc.reads)
	})
}

func newIngestRouter(t *testing.T, svc ingest.Service, conf ingest.Config) *mux.Router {
	t.Helper()

	controller := ingest.NewController(log.NewTestLogger(), svc, conf, nil)
	router := mux.NewRouter()
	controller.AppendRoutes(router)
	return router
}

func TestIngestFile_CrossOrigin(t *testing.T) {
	conf := ingest.Config{AllowedOrigins: []string{"https://ops.example.com"}}

	cases := []struct {
		name        string
		origin      string
		contentType string
		status      int
		reads       int
		allowOrigin string
	}{
		{
			// A browser can send this without a preflight (CSRF); refuse it outright.
			name:        "cross-origin simple text/plain POST is rejected",
			origin:      "https://evil.example",
			contentType: "text/plain;charset=UTF-8",
			status:      http.StatusForbidden,
		},
		{
			name:   "cross-origin POST without Content-Type is rejected",
			origin: "https://evil.example",
			status: http.StatusForbidden,
		},
		{
			name:        "cross-origin POST with non-simple Content-Type but unknown origin is rejected",
			origin:      "https://evil.example",
			contentType: "text/csv",
			status:      http.StatusForbidden,
		},
		{
			name:        "null origin is rejected",
			origin:      "null",
			contentType: "text/csv",
			status:      http.StatusForbidden,
		},
		{
			name:        "same-origin text/plain is rejected (forces preflight for every browser caller)",
			origin:      "http://example.com",
			contentType: "text/plain",
			status:      http.StatusUnsupportedMediaType,
		},
		{
			name:        "form-urlencoded without Origin is rejected",
			contentType: "application/x-www-form-urlencoded",
			status:      http.StatusUnsupportedMediaType,
		},
		{
			name:        "multipart without Origin is rejected",
			contentType: "multipart/form-data; boundary=xyz",
			status:      http.StatusUnsupportedMediaType,
		},
		{
			name:   "missing Content-Type without Origin is rejected",
			status: http.StatusUnsupportedMediaType,
		},
		{
			name:        "non-browser text/csv is accepted",
			contentType: "text/csv",
			status:      http.StatusOK,
			reads:       1,
		},
		{
			name:        "non-browser application/octet-stream is accepted",
			contentType: "application/octet-stream",
			status:      http.StatusOK,
			reads:       1,
		},
		{
			name:        "same-origin text/csv is accepted without a CORS header",
			origin:      "http://example.com",
			contentType: "application/json",
			status:      http.StatusOK,
			reads:       1,
		},
		{
			name:        "allowlisted origin is accepted and echoed",
			origin:      "https://ops.example.com",
			contentType: "application/gzip",
			status:      http.StatusOK,
			reads:       1,
			allowOrigin: "https://ops.example.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &recordingIngestService{}
			router := newIngestRouter(t, svc, conf)

			req := httptest.NewRequest("POST", "/v2/ingest/custom", strings.NewReader("name\nNobody\n"))
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.Equal(t, tc.reads, svc.reads, "list must not be replaced on a rejected request")
			require.Equal(t, tc.allowOrigin, w.Header().Get("Access-Control-Allow-Origin"))
			require.NotEqual(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
		})
	}
}

func TestIngestFile_AllowedOriginsEnv(t *testing.T) {
	t.Setenv("INGEST_ALLOWED_ORIGINS", "https://a.example, https://b.example")

	svc := &recordingIngestService{}
	router := newIngestRouter(t, svc, ingest.Config{})

	req := httptest.NewRequest("POST", "/v2/ingest/custom", strings.NewReader("name\nNobody\n"))
	req.Header.Set("Origin", "https://b.example")
	req.Header.Set("Content-Type", "text/csv")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "https://b.example", w.Header().Get("Access-Control-Allow-Origin"))
	require.Equal(t, "Origin", w.Header().Get("Vary"))
}

func TestIngest_Preflight(t *testing.T) {
	conf := ingest.Config{AllowedOrigins: []string{"https://ops.example.com"}}
	router := newIngestRouter(t, &recordingIngestService{}, conf)

	t.Run("allowlisted origin", func(t *testing.T) {
		req := httptest.NewRequest("OPTIONS", "/v2/ingest/custom", nil)
		req.Header.Set("Origin", "https://ops.example.com")
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "content-type")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusNoContent, w.Code)
		require.Equal(t, "https://ops.example.com", w.Header().Get("Access-Control-Allow-Origin"))
		require.Equal(t, "POST, OPTIONS", w.Header().Get("Access-Control-Allow-Methods"))
		require.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "Content-Type")
	})

	t.Run("unknown origin gets no CORS headers", func(t *testing.T) {
		req := httptest.NewRequest("OPTIONS", "/v2/export/custom", nil)
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Access-Control-Request-Method", "GET")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusNoContent, w.Code)
		require.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
		require.Empty(t, w.Header().Get("Access-Control-Allow-Methods"))
	})
}

func TestExportFile_CORS(t *testing.T) {
	conf := ingest.Config{AllowedOrigins: []string{"https://ops.example.com"}}
	router := newIngestRouter(t, &recordingIngestService{}, conf)

	for _, format := range []string{"", "senzing/jsonl"} {
		t.Run("format="+format, func(t *testing.T) {
			t.Run("no wildcard by default", func(t *testing.T) {
				req := httptest.NewRequest("GET", "/v2/export/custom?format="+format, nil)
				req.Header.Set("Origin", "https://evil.example")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)

				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				require.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
			})

			t.Run("allowlisted origin is echoed", func(t *testing.T) {
				req := httptest.NewRequest("GET", "/v2/export/custom?format="+format, nil)
				req.Header.Set("Origin", "https://ops.example.com")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)

				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				require.Equal(t, "https://ops.example.com", w.Header().Get("Access-Control-Allow-Origin"))
			})
		})
	}
}

// gzipBomb returns a gzip body whose plaintext is a CSV of `rows` identical rows.
func gzipBomb(t *testing.T, rows int) []byte {
	t.Helper()

	var plain bytes.Buffer
	plain.WriteString("name,id\n")
	for i := 0; i < rows; i++ {
		plain.WriteString("Nobody Anyone,0000000000000000000000000000000000000000000000000000000000000\n")
	}
	fd := compress.GzipTestFile(t, &plain)
	bs, err := io.ReadAll(fd)
	require.NoError(t, err)
	return bs
}

func csvIngestConfig(overrides ingest.Config) ingest.Config {
	overrides.Files = map[string]ingest.File{
		"custom": {
			Format: ingest.FormatCSV,
			Mapping: ingest.Mapping{
				Name:     ingest.ColumnDef{Column: "name"},
				SourceID: ingest.ColumnDef{Column: "id"},
				Type:     ingest.Type{Default: "person"},
			},
		},
	}
	return overrides
}

func TestIngestFile_GzipBomb(t *testing.T) {
	// Compressed body is ~ a few KB but inflates to several MiB.
	body := gzipBomb(t, 100_000)
	require.Less(t, len(body), 64<<10, "bomb should compress well")

	t.Run("decompressed bytes over cap return 413", func(t *testing.T) {
		conf := csvIngestConfig(ingest.Config{MaxDecompressedBytes: 1 << 20})
		svc := ingest.NewService(log.NewTestLogger(), conf, &ingest.MockRepository{})
		router := newIngestRouter(t, svc, conf)

		req := httptest.NewRequest("POST", "/v2/ingest/custom", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/gzip")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "decompressed request body exceeds 1048576 byte limit")
	})

	t.Run("entity count over cap returns 413", func(t *testing.T) {
		conf := csvIngestConfig(ingest.Config{MaxEntities: 1000})
		svc := ingest.NewService(log.NewTestLogger(), conf, &ingest.MockRepository{})
		router := newIngestRouter(t, svc, conf)

		req := httptest.NewRequest("POST", "/v2/ingest/custom", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/gzip")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "upload exceeds 1000 entity limit")
	})

	t.Run("small gzip upload under both caps succeeds", func(t *testing.T) {
		conf := csvIngestConfig(ingest.Config{MaxDecompressedBytes: 1 << 20, MaxEntities: 1000})
		svc := ingest.NewService(log.NewTestLogger(), conf, &ingest.MockRepository{})
		router := newIngestRouter(t, svc, conf)

		req := httptest.NewRequest("POST", "/v2/ingest/custom", bytes.NewReader(gzipBomb(t, 5)))
		req.Header.Set("Content-Type", "application/gzip")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
	})
}

func TestIngestFile_SenzingGzipBomb(t *testing.T) {
	var plain bytes.Buffer
	for i := 0; i < 20_000; i++ {
		fmt.Fprintf(&plain, `{"RECORD_ID":"%d","NAME_FULL":"Jane Doe","RECORD_TYPE":"PERSON"}`+"\n", i)
	}
	plainLen := plain.Len()
	fd := compress.GzipTestFile(t, &plain)
	body, err := io.ReadAll(fd)
	require.NoError(t, err)
	require.Less(t, len(body), plainLen/5, "jsonl should compress well")

	conf := ingest.Config{
		Files:                map[string]ingest.File{"senzing-persons": {Format: ingest.FormatSenzing}},
		MaxDecompressedBytes: 256 << 10, // 256 KiB, far below the inflated size
	}
	svc := ingest.NewService(log.NewTestLogger(), conf, &ingest.MockRepository{})
	router := newIngestRouter(t, svc, conf)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v2/ingest/senzing-persons", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/gzip")
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "decompressed request body exceeds")
}

func TestIngestFile_SenzingEntityCap(t *testing.T) {
	conf := ingest.Config{
		Files:       map[string]ingest.File{"sz": {Format: ingest.FormatSenzing}},
		MaxEntities: 2,
	}
	svc := ingest.NewService(log.NewTestLogger(), conf, &ingest.MockRepository{})
	router := newIngestRouter(t, svc, conf)

	var body strings.Builder
	for i := 0; i < 3; i++ {
		body.WriteString(`{"DATA_SOURCE":"sz","RECORD_ID":"` + strings.Repeat("a", i+1) + `","RECORD_TYPE":"PERSON","NAME_FULL":"Nobody"}` + "\n")
	}

	req := httptest.NewRequest("POST", "/v2/ingest/sz", strings.NewReader(body.String()))
	req.Header.Set("Content-Type", "application/x-ndjson")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "upload exceeds 2 entity limit")

	// sanity check the senzing reader option directly
	_, err := senzing.ReadEntitiesWithOptions(strings.NewReader(body.String()), pubsearch.SourceList("sz"), senzing.ReadOptions{MaxEntities: 2})
	require.ErrorIs(t, err, senzing.ErrTooManyEntities)
}
