// Copyright 2022 The Moov Authors
// Use of this source code is governed by an Apache License
// license that can be found in the LICENSE file.

package main

import (
	"cmp"
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/moov-io/base/log"
	"github.com/moov-io/watchman/internal/download"
)

func setupPeriodicRefreshing(ctx context.Context, logger log.Logger, errs chan error, conf download.Config, r *download.Refresher) error {
	refreshEvery := getRefreshInterval(conf)
	maxStaleness := getMaxStaleness(conf, refreshEvery)
	r.SetMaxStaleness(maxStaleness)

	// Initial, blocking load. A failure here is fatal to startup — we have no data yet.
	if err := r.RefreshNow(ctx, download.TriggerStartup); err != nil {
		return err
	}

	// Setup periodic refreshing
	ticker := time.NewTicker(refreshEvery)
	logger.Info().Logf("refreshing data every %v (data reported stale after %v without a successful refresh)", refreshEvery, maxStaleness)

	go func() {
		defer ticker.Stop() // stop ticker only when we're shutdown

		for {
			select {
			case <-ctx.Done():
				errs <- nil
				return

			case <-ticker.C:
				// A manual refresh may be in progress; skip this tick rather than fail.
				//
				// Download/parse failures must not take down a healthy process. After a
				// successful startup we already have a searchable index; a flaky origin
				// or temporary network blip should leave that data in place and retry
				// on the next interval. Status is still recorded on the Refresher
				// (StateFailed + LastError) for operators and GET /v2/data/refresh.
				err := r.RefreshNow(ctx, download.TriggerScheduled)
				if err == nil || errors.Is(err, download.ErrAlreadyRunning) {
					continue
				}
				st := r.Status()
				age := "unknown"
				if st.LastSuccessAt != nil {
					age = time.Since(*st.LastSuccessAt).Round(time.Second).String()
				}
				logger.Error().
					Set("consecutiveFailures", log.Int(st.ConsecutiveFailures)).
					Set("dataAge", log.String(age)).
					Set("stale", log.Bool(st.Stale)).
					LogErrorf("scheduled data refresh failed (keeping previous data): %v", err)
				if st.Stale {
					logger.Error().LogErrorf("sanctions data is STALE: last successful refresh %s ago exceeds max staleness %v - screening results are being served from outdated lists", age, maxStaleness)
				}
			}
		}
	}()

	return nil
}

const (
	defaultRefreshInterval = 12 * time.Hour
)

func getRefreshInterval(conf download.Config) time.Duration {
	override := strings.TrimSpace(os.Getenv("DATA_REFRESH_INTERVAL"))
	if override != "" {
		dur, err := time.ParseDuration(override)
		if err == nil {
			return dur
		}
	}
	return cmp.Or(conf.RefreshInterval, defaultRefreshInterval)
}

// getMaxStaleness returns how old served data may be before it is reported as stale.
// DATA_MAX_STALENESS overrides Download.MaxStaleness; the default is twice the refresh
// interval so a single missed refresh alerts but does not page.
func getMaxStaleness(conf download.Config, refreshEvery time.Duration) time.Duration {
	override := strings.TrimSpace(os.Getenv("DATA_MAX_STALENESS"))
	if override != "" {
		dur, err := time.ParseDuration(override)
		if err == nil && dur > 0 {
			return dur
		}
	}
	if conf.MaxStaleness > 0 {
		return conf.MaxStaleness
	}
	return 2 * refreshEvery
}
