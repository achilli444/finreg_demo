package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/moov-io/base/log"
	"github.com/moov-io/watchman/internal/download"
	internalsearch "github.com/moov-io/watchman/internal/search"
	"github.com/moov-io/watchman/pkg/search"
)

// channelFrom maps the request source Watchman stamps on every query
// (api-request / mcp-request) to the audit channel label.
func channelFrom(q search.Entity[search.Value]) string {
	switch q.Source {
	case search.SourceAPIRequest:
		return ChannelAPI
	case search.SourceMCPRequest:
		return ChannelMCP
	}
	if q.Source != "" {
		return string(q.Source)
	}
	return "unknown"
}

// Service wraps a search.Service and emits one audit Event per Search call,
// including failed calls. Audit write failures are logged but never fail the
// search, so screening keeps working while operators fix the sink.
type Service struct {
	internalsearch.Service

	logger log.Logger
	cfg    Config
	sink   Sink
}

// Wrap decorates inner with audit logging.
func Wrap(logger log.Logger, cfg Config, sink Sink, inner internalsearch.Service) *Service {
	return &Service{Service: inner, logger: logger, cfg: cfg, sink: sink}
}

func (s *Service) Search(ctx context.Context, query search.Entity[search.Value], opts internalsearch.SearchOpts) ([]search.SearchedEntity[search.Value], error) {
	start := time.Now()
	results, err := s.Service.Search(ctx, query, opts)
	s.record(start, query, opts, results, err, s.Service.LatestStats())
	return results, err
}

func (s *Service) record(start time.Time, query search.Entity[search.Value], opts internalsearch.SearchOpts, results []search.SearchedEntity[search.Value], searchErr error, stats download.Stats) {
	ev := Event{
		Timestamp:   start.UTC(),
		RequestID:   opts.RequestID,
		Channel:     channelFrom(query),
		Algorithm:   algorithmName(opts.Algorithm),
		MinMatch:    opts.MinMatch,
		Limit:       opts.Limit,
		QueryHash:   hashQuery(query),
		ListInfo:    stats.ListInfo(),
		ResultCount: len(results),
		Results:     make([]Result, 0, len(results)),
		DurationMs:  time.Since(start).Milliseconds(),
	}
	if s.cfg.IncludeQuery {
		q := query
		ev.Query = &q
	}
	if searchErr != nil {
		ev.Error = searchErr.Error()
	}
	for i := range results {
		r := results[i]
		if r.Match > ev.TopMatch {
			ev.TopMatch = r.Match
		}
		res := Result{
			SourceList: string(r.Source),
			SourceID:   r.SourceID,
			Name:       r.Name,
			Type:       r.Type,
			Match:      r.Match,
		}
		if s.cfg.IncludeDetails && len(r.Details.Pieces) > 0 {
			d := r.Details
			res.Details = &d
		}
		ev.Results = append(ev.Results, res)
	}

	if err := s.sink.Write(ev); err != nil {
		s.logger.Error().Set("requestID", log.String(opts.RequestID)).LogErrorf("AUDIT WRITE FAILED - screening event not persisted: %v", err)
	}
}

func algorithmName(alg search.StringMatchAlgorithm) string {
	if alg == "" {
		return "jaro-winkler"
	}
	return string(alg)
}

// hashQuery gives a stable fingerprint of the normalized query so events can be
// correlated with an input the integrator retains, without storing PII here.
func hashQuery(q search.Entity[search.Value]) string {
	bs, err := json.Marshal(q)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(bs)
	return hex.EncodeToString(sum[:])
}
