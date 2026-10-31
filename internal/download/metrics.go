package download

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Published on the admin server's /metrics so a refresh that silently lost a
// list (IgnoredDownloadErrors) is visible to operators and alerting.
var (
	ignoredListErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchman",
		Subsystem: "data_refresh",
		Name:      "ignored_list_errors_total",
		Help:      "Download or parse failures suppressed because the list is in IgnoredDownloadErrors, by list",
	}, []string{"list"})

	refreshDegraded = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "watchman",
		Subsystem: "data_refresh",
		Name:      "degraded",
		Help:      "1 when the most recent data refresh completed with ignored list failures, otherwise 0",
	})
)
