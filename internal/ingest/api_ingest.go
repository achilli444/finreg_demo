package ingest

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/moov-io/base/log"
	"github.com/moov-io/base/telemetry"
	"github.com/moov-io/watchman/internal/api"
	pubsearch "github.com/moov-io/watchman/pkg/search"
	"github.com/moov-io/watchman/pkg/sources/senzing"

	"github.com/gorilla/mux"
	"go.opentelemetry.io/otel/attribute"
)

type Controller interface {
	AppendRoutes(router *mux.Router) *mux.Router
}

// IndexCache is the in-memory ingest search index. Optional; tests may pass nil.
type IndexCache interface {
	RefreshIngest(ctx context.Context) error
}

func NewController(logger log.Logger, service Service, conf Config, cache IndexCache) Controller {
	return &controller{
		logger:  logger,
		service: service,
		conf:    conf,
		cache:   cache,
	}
}

type controller struct {
	logger  log.Logger
	service Service
	conf    Config
	cache   IndexCache
}

func (c *controller) AppendRoutes(router *mux.Router) *mux.Router {
	router.
		Name("ingest-file").
		Methods("POST").
		Path("/v2/ingest/{fileType}").
		HandlerFunc(c.ingestFile)

	router.
		Name("export-file").
		Methods("GET").
		Path("/v2/export/{fileType}").
		HandlerFunc(c.exportFile)

	// CORS preflight for browsers on the configured origin allowlist
	router.
		Name("ingest-file-preflight").
		Methods("OPTIONS").
		Path("/v2/ingest/{fileType}").
		HandlerFunc(c.preflight("POST"))

	router.
		Name("export-file-preflight").
		Methods("OPTIONS").
		Path("/v2/export/{fileType}").
		HandlerFunc(c.preflight("GET"))

	return router
}

// corsSafelistedContentTypes are media types a browser sends cross-origin without a
// preflight (https://fetch.spec.whatwg.org/#cors-safelisted-request-header). Refusing
// them on state-changing routes forces a preflight, which only allowlisted origins pass.
var corsSafelistedContentTypes = map[string]bool{
	"text/plain":                        true,
	"application/x-www-form-urlencoded": true,
	"multipart/form-data":               true,
}

var (
	errOriginNotAllowed  = errors.New("cross-origin requests are not allowed from this origin")
	errMissingMediaType  = errors.New("Content-Type is required, for example text/csv, application/json or application/gzip")
	errUnsupportedMedia  = errors.New("Content-Type must not be text/plain, application/x-www-form-urlencoded or multipart/form-data")
	errUnparsableMedia   = errors.New("Content-Type could not be parsed")
	preflightMaxAgeValue = "600"
)

// rejectCrossSite blocks browser requests from origins that are neither the server's own
// origin nor on the configured allowlist. Requests without an Origin header come from
// non-browser clients and are passed through.
func (c *controller) rejectCrossSite(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || sameOrigin(origin, r) || c.conf.originAllowed(origin) {
		return false
	}
	api.ErrorResponseStatus(w, http.StatusForbidden, errOriginNotAllowed)
	return true
}

// requireNonSimpleContentType rejects bodies a browser can send without a preflight.
func requireNonSimpleContentType(w http.ResponseWriter, r *http.Request) bool {
	ct := strings.TrimSpace(r.Header.Get("Content-Type"))
	if ct == "" {
		api.ErrorResponseStatus(w, http.StatusUnsupportedMediaType, errMissingMediaType)
		return true
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		api.ErrorResponseStatus(w, http.StatusUnsupportedMediaType, errUnparsableMedia)
		return true
	}
	if corsSafelistedContentTypes[strings.ToLower(mediaType)] {
		api.ErrorResponseStatus(w, http.StatusUnsupportedMediaType, errUnsupportedMedia)
		return true
	}
	return false
}

func sameOrigin(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// applyCORS echoes an allowlisted Origin back so that browser may read the response.
// No Access-Control-Allow-Origin header is written for any other origin.
func (c *controller) applyCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || !c.conf.originAllowed(origin) {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Add("Vary", "Origin")
	return true
}

func (c *controller) preflight(method string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c.applyCORS(w, r) {
			w.Header().Set("Access-Control-Allow-Methods", method+", OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type")
			w.Header().Set("Access-Control-Max-Age", preflightMaxAgeValue)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (c *controller) ingestFile(w http.ResponseWriter, r *http.Request) {
	fileType := api.CleanUserInput(mux.Vars(r)["fileType"])
	if fileType == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	ctx, span := telemetry.StartSpan(context.Background(), "api-ingest-file")
	defer span.End()

	span.SetAttributes(attribute.String("file_type", fileType))

	logger := c.logger.With(log.Fields{
		"file_type": log.String(fileType),
	})

	if c.rejectCrossSite(w, r) {
		logger.Info().Log("rejecting cross-origin ingest request")
		return
	}
	if requireNonSimpleContentType(w, r) {
		logger.Info().Log("rejecting ingest request without a non-form Content-Type")
		return
	}
	c.applyCORS(w, r)

	maxBytes := c.conf.maxBodyBytes()
	if r.ContentLength > maxBytes {
		logger.Info().Logf("rejecting ingest body: content-length %d exceeds %d byte limit", r.ContentLength, maxBytes)
		api.ErrorResponseStatus(w, http.StatusRequestEntityTooLarge, errRequestBodyTooLarge(maxBytes))
		return
	}

	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		defer r.Body.Close()
	}

	parsedFile, err := c.service.ReadEntitiesFromFile(ctx, fileType, r.Body)
	if err != nil {
		if isMaxBytesError(err) {
			logger.Error().Log("ingest request body too large")
			api.ErrorResponseStatus(w, http.StatusRequestEntityTooLarge, errRequestBodyTooLarge(maxBytes))
			return
		}
		if errors.Is(err, ErrDecompressedBodyTooLarge) {
			logger.Error().Log("ingest request body too large after decompression")
			api.ErrorResponseStatus(w, http.StatusRequestEntityTooLarge, errDecompressedTooLarge(c.conf.maxDecompressedBytes()))
			return
		}
		if errors.Is(err, ErrTooManyEntities) {
			logger.Error().Log("ingest request has too many entities")
			api.ErrorResponseStatus(w, http.StatusRequestEntityTooLarge, errTooManyEntities(c.conf.maxEntities()))
			return
		}
		// file_type is already attached as a structured field; avoid interpolating
		// user-controlled values into the log message (CWE-117 / go/log-injection).
		logger.Error().Log("problem reading entities from file")
		api.ErrorResponse(w, err)
		return
	}

	logger.Info().Logf("found %d entities from file", len(parsedFile.Entities))

	// Save the parsed entities
	err = c.service.ReplaceEntities(ctx, parsedFile.FileType, parsedFile.Entities)
	if err != nil {
		logger.Error().Log("problem updating entities")
		api.ErrorResponse(w, err)
		return
	}

	if c.cache != nil {
		if err := c.cache.RefreshIngest(ctx); err != nil {
			span.RecordError(err)
			logger.Error().Logf("problem refreshing ingest search index: %v", err)
			api.ErrorResponse(w, err)
			return
		}
	}

	// Marshal the response
	err = api.WriteJSON(w, pubsearch.IngestFileResponse{
		FileType: parsedFile.FileType,
		Entities: parsedFile.Entities,
	})
	if err != nil {
		span.RecordError(err)
	}
}

func (c *controller) exportFile(w http.ResponseWriter, r *http.Request) {
	ctx, span := telemetry.StartSpan(context.Background(), "api-export-senzing")
	defer span.End()

	fileType := api.CleanUserInput(mux.Vars(r)["fileType"])
	if fileType == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	queryParams := api.NewQueryParams(r.URL)
	outputFormat, subformat := api.ChooseEntityFormat(r.Header, queryParams.Get("format"))

	span.SetAttributes(
		attribute.String("file_type", fileType),
		attribute.String("output_format", string(outputFormat)),
	)

	logger := c.logger.With(log.Fields{
		"file_type":     log.String(fileType),
		"output_format": log.String(string(outputFormat)),
	})

	// Read the entities
	entities, err := c.service.GetEntitiesBySource(ctx, fileType)
	if err != nil {
		err = logger.Error().LogErrorf("problem getting entities for export: %v", err).Err()
		span.RecordError(err)

		api.ErrorResponse(w, err)
		return
	}
	logger.Info().Logf("exporting %d entities to senzing format", len(entities))

	c.applyCORS(w, r)

	switch outputFormat {
	case api.EntityWatchman:
		err = api.WriteJSON(w, entities)

	case api.EntitySenzing:
		w.Header().Set("Content-Type", "application/x-ndjson")

		opts := senzing.ExportOptions{
			DataSource: fileType,
			Format:     subformat,
		}
		err = senzing.WriteEntities(w, entities, opts)
	}
	if err != nil {
		err = logger.Error().LogErrorf("problem rendering export response into %v", outputFormat).Err()
		span.RecordError(err)

		api.ErrorResponse(w, err)
		return
	}
}

func errRequestBodyTooLarge(maxBytes int64) error {
	return fmt.Errorf("request body exceeds %d byte limit", maxBytes)
}

func errDecompressedTooLarge(maxBytes int64) error {
	return fmt.Errorf("decompressed request body exceeds %d byte limit", maxBytes)
}

func errTooManyEntities(maxEntities int) error {
	return fmt.Errorf("upload exceeds %d entity limit", maxEntities)
}

func isMaxBytesError(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}
