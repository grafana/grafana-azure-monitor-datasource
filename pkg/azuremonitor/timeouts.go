package azuremonitor

import (
	"time"

	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/types"
)

const (
	defaultLogsTimeout          = 3 * time.Minute
	defaultMetricsTimeout       = 30 * time.Second
	defaultResourceGraphTimeout = 30 * time.Second
)

// legacyTimeout extracts the deprecated top-level "timeout" setting (in seconds), if set.
func legacyTimeout(jsonData map[string]any) *time.Duration {
	v, ok := jsonData["timeout"]
	if !ok {
		return nil
	}
	seconds, ok := v.(float64)
	if !ok || seconds < 0 {
		return nil
	}
	d := time.Duration(seconds * float64(time.Second))
	return &d
}

func durationFromSeconds(seconds *float64) *time.Duration {
	if seconds == nil || *seconds < 0 {
		return nil
	}
	d := time.Duration(*seconds * float64(time.Second))
	return &d
}

// resolveTimeout returns the effective HTTP client timeout for a route. New
// per-service settings take precedence, falling back to the deprecated shared
// "timeout" setting, then to a per-service default. Routes not covered by
// logs/metrics/resource graph return 0, meaning "no override" - the caller
// should keep using whatever settings.HTTPClientOptions already resolved.
func resolveTimeout(routeName string, settings types.AzureMonitorSettings, legacy *time.Duration) time.Duration {
	var perService *time.Duration
	var fallbackDefault time.Duration

	switch routeName {
	case types.RouteAzureLogAnalytics:
		perService = durationFromSeconds(settings.LogsTimeout)
		fallbackDefault = defaultLogsTimeout
	case types.RouteAzureMonitor, types.RouteAzureMonitorBatchMetrics:
		perService = durationFromSeconds(settings.MetricsTimeout)
		fallbackDefault = defaultMetricsTimeout
	case types.RouteAzureResourceGraph:
		perService = durationFromSeconds(settings.ResourceGraphTimeout)
		fallbackDefault = defaultResourceGraphTimeout
	default:
		return 0
	}

	if perService != nil {
		return *perService
	}
	if legacy != nil {
		return *legacy
	}
	return fallbackDefault
}
