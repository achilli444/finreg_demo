package download_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func TestClient_GetFiles_PartialFailure(t *testing.T) {
	logger := log.NewTestLogger()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "sdn.csv", "alt.csv", "sdn_comments.csv":
			w.Write([]byte("ok"))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)

	dl := download.New(logger, server.Client())
	require.NotNil(t, dl)

	namesAndSources := map[string]string{
		"sdn.csv":          server.URL + "/sdn.csv",
		"add.csv":          server.URL + "/add.csv",
		"alt.csv":          server.URL + "/alt.csv",
		"sdn_comments.csv": server.URL + "/sdn_comments.csv",
	}

	files, err := dl.GetFiles(context.Background(), "", namesAndSources)
	require.Error(t, err)
	require.Nil(t, files)
	require.ErrorContains(t, err, "1 of 4 files could not be obtained")
	require.ErrorContains(t, err, "downloading add.csv")
	require.ErrorContains(t, err, "unexpected HTTP status 500")
}

func TestClient_GetFiles_AllFilesDownloaded(t *testing.T) {
	logger := log.NewTestLogger()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(filepath.Base(r.URL.Path)))
	}))
	t.Cleanup(server.Close)

	dl := download.New(logger, server.Client())

	namesAndSources := map[string]string{
		"sdn.csv": server.URL + "/sdn.csv",
		"add.csv": server.URL + "/add.csv",
	}

	files, err := dl.GetFiles(context.Background(), "", namesAndSources)
	require.NoError(t, err)
	require.Len(t, files, 2)
	require.NoError(t, files.Close())
}
