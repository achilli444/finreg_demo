package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

func TestDownloadMetrics_RecordLists(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewDownloadMetrics(reg)

	m.RecordLists(map[string]int{"us_ofac": 120, "us_non_sdn": 7})

	got := gatherDownload(t, reg)
	require.Equal(t, map[string]float64{"us_ofac": 120, "us_non_sdn": 7}, got.perList)
	require.Equal(t, float64(2), got.listsLoaded)

	// A later refresh that drops a list must remove its series, not leave it stale.
	m.RecordLists(map[string]int{"us_ofac": 121})

	got = gatherDownload(t, reg)
	require.Equal(t, map[string]float64{"us_ofac": 121}, got.perList)
	require.Equal(t, float64(1), got.listsLoaded)
}

type downloadGather struct {
	perList     map[string]float64
	listsLoaded float64
}

func gatherDownload(t *testing.T, reg prometheus.Gatherer) downloadGather {
	t.Helper()

	families, err := reg.Gather()
	require.NoError(t, err)

	out := downloadGather{perList: map[string]float64{}}
	for _, fam := range families {
		switch fam.GetName() {
		case "watchman_download_list_entities":
			for _, metric := range fam.GetMetric() {
				out.perList[labelValue(metric, "list")] = metric.GetGauge().GetValue()
			}
		case "watchman_download_lists_loaded":
			require.Len(t, fam.GetMetric(), 1)
			out.listsLoaded = fam.GetMetric()[0].GetGauge().GetValue()
		}
	}
	return out
}

func labelValue(metric *dto.Metric, name string) string {
	for _, lp := range metric.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}
