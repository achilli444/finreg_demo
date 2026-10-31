// Package audit emits one structured, append-only record per screening request so
// a reviewer can later reconstruct what was screened, against which list versions,
// with which algorithm and threshold, and what was returned.
//
// Watchman is a screening engine, not a system of record: the integrating
// application still owns case management, regulatory reporting and the
// authoritative retention schedule. This package gives that application a
// complete, machine-readable event per request (see docs/compliance.md).
package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/moov-io/base/log"
	"github.com/moov-io/watchman/pkg/search"
)

// Config controls screening audit logging.
type Config struct {
	// Enabled turns on one audit event per search (HTTP and MCP).
	Enabled bool

	// Directory receives append-only JSON Lines files named
	// screening-YYYY-MM-DD.jsonl (UTC). When empty, events are written to the
	// process log instead, for environments that ship stdout to a SIEM.
	Directory string

	// RetentionDays deletes daily files older than this many days on each
	// rotation. Zero keeps files forever (the integrator prunes). BSA/AML
	// record-keeping expects at least 5 years (31 CFR 1010.430); GDPR/UK GDPR
	// storage limitation expects a documented, finite period.
	RetentionDays int

	// IncludeQuery controls whether the full query entity (customer PII) is
	// written. When false only a SHA-256 of the normalized query is kept, which
	// still lets a reviewer prove which input produced which result when the
	// integrating application retains the input itself.
	IncludeQuery bool

	// IncludeDetails writes the per-field score breakdown for each hit when the
	// search computed it (debug=true or MCP includeDetails).
	IncludeDetails bool
}

// Channel identifies which API surface issued the search.
const (
	ChannelAPI = "api"
	ChannelMCP = "mcp"
)

// Event is one screening request/response pair.
type Event struct {
	Timestamp time.Time `json:"timestamp"`
	RequestID string    `json:"requestId,omitempty"`
	Channel   string    `json:"channel"`

	// Policy inputs that shaped the result.
	Algorithm string  `json:"algorithm"`
	MinMatch  float64 `json:"minMatch"`
	Limit     int     `json:"limit"`

	// Query is the normalized input entity, or nil when Config.IncludeQuery is false.
	Query     *search.Entity[search.Value] `json:"query,omitempty"`
	QueryHash string                       `json:"queryHash"`

	// ListInfo pins the exact list versions (hashes, counts, refresh time) in use.
	ListInfo *search.ListInfo `json:"listInfo"`

	ResultCount int      `json:"resultCount"`
	TopMatch    float64  `json:"topMatch"`
	Results     []Result `json:"results"`

	DurationMs int64  `json:"durationMs"`
	Error      string `json:"error,omitempty"`
}

// Result is a returned candidate with enough identity to be located again.
type Result struct {
	SourceList string                  `json:"sourceList"`
	SourceID   string                  `json:"sourceId"`
	Name       string                  `json:"name"`
	Type       search.EntityType       `json:"type"`
	Match      float64                 `json:"match"`
	Details    *search.SimilarityScore `json:"details,omitempty"`
}

// Sink receives finished events. Implementations must be safe for concurrent use.
type Sink interface {
	Write(Event) error
	Close() error
}

// NewSink builds the sink described by cfg: a rotating JSONL directory, or the
// process logger when no directory is configured.
func NewSink(logger log.Logger, cfg Config) (Sink, error) {
	if strings.TrimSpace(cfg.Directory) == "" {
		return &logSink{logger: logger}, nil
	}
	return newFileSink(logger, cfg.Directory, cfg.RetentionDays)
}

type logSink struct {
	logger log.Logger
}

func (s *logSink) Write(ev Event) error {
	bs, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	s.logger.Info().Set("audit", log.String("screening")).Log(string(bs))
	return nil
}

func (s *logSink) Close() error { return nil }

const filePrefix = "screening-"

type fileSink struct {
	logger        log.Logger
	dir           string
	retentionDays int

	mu      sync.Mutex
	current *os.File
	day     string
}

func newFileSink(logger log.Logger, dir string, retentionDays int) (*fileSink, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("creating audit directory %s: %w", dir, err)
	}
	return &fileSink{logger: logger, dir: dir, retentionDays: retentionDays}, nil
}

func (s *fileSink) Write(ev Event) error {
	bs, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	bs = append(bs, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	day := ev.Timestamp.UTC().Format("2006-01-02")
	if s.current == nil || day != s.day {
		if err := s.rotate(day); err != nil {
			return err
		}
	}
	_, err = s.current.Write(bs)
	return err
}

func (s *fileSink) rotate(day string) error {
	if s.current != nil {
		s.current.Close()
		s.current = nil
	}
	path := filepath.Join(s.dir, filePrefix+day+".jsonl")
	fd, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640) //nolint:gosec
	if err != nil {
		return fmt.Errorf("opening audit file %s: %w", path, err)
	}
	s.current = fd
	s.day = day
	s.prune(time.Now().UTC())
	return nil
}

// prune removes daily files older than the retention window. It only ever
// deletes files this sink named, never anything else in the directory.
func (s *fileSink) prune(now time.Time) {
	if s.retentionDays <= 0 {
		return
	}
	cutoff := now.AddDate(0, 0, -s.retentionDays).Format("2006-01-02")
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		s.logger.Warn().Logf("audit retention: reading %s: %v", s.dir, err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		day, ok := strings.CutPrefix(name, filePrefix)
		if !ok || !strings.HasSuffix(day, ".jsonl") {
			continue
		}
		day = strings.TrimSuffix(day, ".jsonl")
		if day < cutoff {
			if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
				s.logger.Warn().Logf("audit retention: removing %s: %v", name, err)
			} else {
				s.logger.Info().Logf("audit retention: removed %s (older than %d days)", name, s.retentionDays)
			}
		}
	}
}

func (s *fileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return nil
	}
	err := s.current.Close()
	s.current = nil
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

var _ io.Closer = (*fileSink)(nil)
