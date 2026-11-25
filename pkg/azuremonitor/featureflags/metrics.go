package featureflags

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Singleflight followers increment no outcome, so miss and error each count one OFREP request.
const (
	outcomeHit          = "hit"
	outcomeMiss         = "miss"
	outcomeError        = "error"
	outcomeUnconfigured = "unconfigured"
)

var (
	evaluationsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grafana",
		Name:      "azure_monitor_plugin_feature_flag_evaluations_total",
		Help:      "Feature flag evaluations by flag and outcome (hit, miss, error, unconfigured)",
	}, []string{"flag", "outcome"})

	requestDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "grafana",
		Name:      "azure_monitor_plugin_feature_flag_request_duration_seconds",
		Help:      "Duration of OFREP feature flag requests in seconds",
		Buckets:   []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	}, []string{"flag"})
)
