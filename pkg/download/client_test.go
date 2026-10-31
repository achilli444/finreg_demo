package download_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moov-io/base/log"
	"github.com/moov-io/watchman/pkg/download"

	"github.com/stretchr/testify/require"
)

func TestClient_GetFiles_InitialDir(t *testing.T) {
	logger := log.NewTestLogger()

	dl := download.New(logger, nil)
	require.NotNil(t, dl)

	ctx := context.Background()
	dir := filepath.Join("..", "sources", "ofac", "testdata")

	namesAndSources := make(map[string]string)
	namesAndSources["sdn.csv"] = "https://example.com"
	namesAndSources["alt.csv"] = "https://example.com"
	namesAndSources["sdn_comments.csv"] = "https://example.com"

	files, err := dl.GetFiles(ctx, dir, namesAndSources)
	require.NoError(t, err)
	require.Len(t, files, 3)

	require.NotNil(t, files["sdn.csv"])
	require.NotNil(t, files["alt.csv"])
	require.NotNil(t, files["sdn_comments.csv"])
}

func TestClient_DownloadFile(t *testing.T) {
	logger := log.NewTestLogger()

	dl := download.New(logger, nil)
	require.NotNil(t, dl)

	namesAndSources := map[string]string{
		"client": "file://client.go",
		"test":   "file://client_test.go",
	}

	files, err := dl.GetFiles(context.Background(), "", namesAndSources)
	require.NoError(t, err)
	require.Len(t, files, 2)

	require.NotNil(t, files["client"])
	require.NotNil(t, files["test"])

	require.NoError(t, files["client"].Close())
	require.NoError(t, files["test"].Close())
}

func TestClient_GetFiles_PartialFailureIsAnError(t *testing.T) {
	logger := log.NewTestLogger()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/good.csv") {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok\n"))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	dl := download.New(logger, server.Client())
	require.NotNil(t, dl)

	dir := t.TempDir()
	namesAndSources := map[string]string{
		"good.csv":    server.URL + "/good.csv",
		"missing.csv": server.URL + "/missing.csv",
	}

	files, err := dl.GetFiles(context.Background(), dir, namesAndSources)
	require.Error(t, err)
	require.Nil(t, files, "a partial file set must not be returned as success")
	require.Contains(t, err.Error(), "1 of 2 files failed to download")
	require.Contains(t, err.Error(), "missing.csv")
}
