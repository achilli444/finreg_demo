package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// DownloadMetrics publishes which source lists the most recent successful data
// refresh loaded and how many entities each contributed. Operators can alert when
// a required list (us_ofac, us_non_sdn) is missing or reports zero entities.
type DownloadMetrics struct {
	listEntities *prometheus.GaugeVec
	listsLoaded  prometheus.Gauge
}

// NewDownloadMetrics registers the download collectors against reg. Pass
// prometheus.DefaultRegisterer to have them served by the admin server.
func NewDownloadMetrics(reg prometheus.Registerer) *DownloadMetrics {
	return &DownloadMetrics{
		listEntities: promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "watchman",
			Subsystem: "download",
			Name:      "list_entities",
			Help:      "Entities loaded per source list by the most recent successful data refresh",
		}, []string{"list"}),
		listsLoaded: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
			Namespace: "watchman",
			Subsystem: "download",
			Name:      "lists_loaded",
			Help:      "Number of source lists loaded by the most recent successful data refresh",
		}),
	}
}

// RecordLists replaces the published list set with lists (list name -> entity
// count). Lists absent from the latest refresh are removed rather than left at
// their previous value, so a list that stops loading disappears from /metrics.
func (m *DownloadMetrics) RecordLists(lists map[string]int) {
	m.listEntities.Reset()
	for name, count := range lists {
		m.listEntities.WithLabelValues(name).Set(float64(count))
	}
	m.listsLoaded.Set(float64(len(lists)))
}
