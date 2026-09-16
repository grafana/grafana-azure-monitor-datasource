package azuremonitor

import (
	"net/http"
	"regexp"
	"strings"
)

// allowedResourcePaths enumerates, per backend route, the exact shape of
// ARM/Log Analytics/Resource Graph calls the frontend actually issues
// through the resource proxy (see /public/app/plugins/datasource/azuremonitor:
// url_builder.ts, azure_monitor_datasource.ts, azure_log_analytics_datasource.ts,
// azure_resource_graph_datasource.ts). See RESOURCE_PROXY.md for why this
// exists and why each pattern anchors on a fixed operation suffix rather
// than the (arbitrary, user-chosen) resource identity that precedes it.
var allowedResourcePaths = map[string][]struct {
	method  string
	pattern *regexp.Regexp
}{
	azureMonitor: {
		{http.MethodGet, regexp.MustCompile(`^/subscriptions$`)},
		{http.MethodGet, regexp.MustCompile(`^/subscriptions/[^/%]+/locations$`)},
		{http.MethodGet, regexp.MustCompile(`(?i)^/subscriptions/[^/%]+/providers/microsoft\.operationalinsights/workspaces$`)},
		{http.MethodGet, regexp.MustCompile(`(?i)^/subscriptions/[^%]+/providers/microsoft\.insights/metricnamespaces$`)},
		{http.MethodGet, regexp.MustCompile(`(?i)^/subscriptions/[^%]+/providers/microsoft\.insights/metricdefinitions$`)},
		{http.MethodGet, regexp.MustCompile(`(?i)^/subscriptions/[^%]+/providers/microsoft\.operationalinsights/workspaces/[^/%]+/tables/[^/%]+$`)},
	},
	azureLogAnalytics: {
		{http.MethodGet, regexp.MustCompile(`^/v1/metadata$`)},
		{http.MethodGet, regexp.MustCompile(`^/v1/subscriptions/[^%]+/metadata$`)},
		{http.MethodPost, regexp.MustCompile(`^/usage/basiclogs$`)},
	},
	azureResourceGraph: {
		{http.MethodPost, regexp.MustCompile(`(?i)^/providers/microsoft\.resourcegraph/resources$`)},
	},
}

// isAllowedResourcePath reports whether method+path (the ARM-side request,
// after the /azuremonitor|/loganalytics|/resourcegraph prefix has been
// stripped by getTarget, and without its query string) is one of the calls
// the datasource is designed to make on subDataSource. path is
// req.URL.Path, already percent-decoded once by net/http. See
// RESOURCE_PROXY.md for why "%", dot-segments, and empty segments are
// rejected outright below rather than left to the shape patterns.
func isAllowedResourcePath(subDataSource, method, path string) bool {
	// A "%" here can only be an escaped escape (net/http already decoded
	// once) -- see RESOURCE_PROXY.md.
	if strings.Contains(path, "%") {
		return false
	}
	// Empty segments come from empty/malformed template variables.
	if strings.Contains(path, "//") {
		return false
	}
	// A literal ".." can reach here without net/http.ServeMux's own
	// cleanup catching it -- see RESOURCE_PROXY.md.
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}

	for _, allowed := range allowedResourcePaths[subDataSource] {
		if allowed.method == method && allowed.pattern.MatchString(path) {
			return true
		}
	}
	return false
}
