package loganalytics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/types"
)

const (
	testComponentA = "/subscriptions/sub-1/resourcegroups/rg/providers/microsoft.insights/components/app-a"
	testComponentB = "/subscriptions/sub-1/resourcegroups/rg/providers/microsoft.insights/components/app-b"
	testComponentC = "/subscriptions/sub-2/resourcegroups/rg/providers/microsoft.insights/components/app-c"
)

func newARGTestDsInfo(url string, client *http.Client) types.DatasourceInfo {
	return types.DatasourceInfo{
		Services: map[string]types.DatasourceService{
			"Azure Resource Graph": {URL: url, HTTPClient: client},
		},
	}
}

func TestListReadableAppInsightsComponents(t *testing.T) {
	var bodies []map[string]any
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.HasSuffix(r.URL.Path, "/providers/Microsoft.ResourceGraph/resources"))
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		require.NoError(t, json.Unmarshal(raw, &body))
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		if _, paged := body["options"].(map[string]any)["$skipToken"]; paged {
			_, _ = io.WriteString(w, fmt.Sprintf(`{"data":[{"id":%q,"subscriptionId":"sub-2"}]}`, testComponentC))
			return
		}
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"data":[{"id":%q,"subscriptionId":"sub-1"},{"id":%q,"subscriptionId":"sub-1"}],"$skipToken":"tok"}`,
			testComponentB, testComponentA))
	}))
	defer svr.Close()

	got, truncated, err := listReadableAppInsightsComponents(context.Background(), newARGTestDsInfo(svr.URL, svr.Client()))
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []appInsightsComponent{
		{ID: testComponentA, SubscriptionID: "sub-1"},
		{ID: testComponentB, SubscriptionID: "sub-1"},
		{ID: testComponentC, SubscriptionID: "sub-2"},
	}, got)

	require.Len(t, bodies, 2)
	_, hasSubscriptions := bodies[0]["subscriptions"]
	require.False(t, hasSubscriptions, "first request must not filter by subscription")
	require.Equal(t, "tok", bodies[1]["options"].(map[string]any)["$skipToken"])
}

func TestListReadableAppInsightsComponents_TruncatesAtCap(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows := make([]string, 0, exemplarComponentCap+1)
		for i := 0; i <= exemplarComponentCap; i++ {
			rows = append(rows, fmt.Sprintf(`{"id":"/subscriptions/s/resourcegroups/rg/providers/microsoft.insights/components/app-%02d","subscriptionId":"s"}`, i))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[`+strings.Join(rows, ",")+`]}`)
	}))
	defer svr.Close()

	got, truncated, err := listReadableAppInsightsComponents(context.Background(), newARGTestDsInfo(svr.URL, svr.Client()))
	require.NoError(t, err)
	require.True(t, truncated)
	require.Len(t, got, exemplarComponentCap)
}

func TestListReadableAppInsightsComponents_GraphError(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"BadRequest"}}`)
	}))
	defer svr.Close()

	_, _, err := listReadableAppInsightsComponents(context.Background(), newARGTestDsInfo(svr.URL, svr.Client()))
	require.Error(t, err)
}

const testOperationID = "d958cc15dfc4fbaafeb3f5d8a04e3d01"

func newLogsTestDsInfo(url string, client *http.Client) types.DatasourceInfo {
	return types.DatasourceInfo{
		Services: map[string]types.DatasourceService{
			"Azure Log Analytics": {URL: url, HTTPClient: client},
		},
	}
}

func testTimeRange() backend.TimeRange {
	from := time.Date(2026, 11, 18, 19, 0, 0, 0, time.UTC)
	return backend.TimeRange{From: from, To: from.Add(time.Hour)}
}

func TestComponentHasOperation(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		operation  string
		expected   componentCheck
		expectHit  bool
		expectErr  bool
		expectSkip bool
	}{
		{
			name:      "row returned means the trace is in this component",
			status:    http.StatusOK,
			body:      `{"tables":[{"name":"PrimaryResult","columns":[{"name":"timestamp","type":"datetime"}],"rows":[["2026-11-18T19:45:00Z"]]}]}`,
			operation: testOperationID,
			expected:  componentHasTrace,
			expectHit: true,
		},
		{
			name:      "no rows means the component does not have it",
			status:    http.StatusOK,
			body:      `{"tables":[{"name":"PrimaryResult","columns":[],"rows":[]}]}`,
			operation: testOperationID,
			expected:  componentAbsent,
			expectHit: true,
		},
		{
			name:       "403 skips the component without failing the lookup",
			status:     http.StatusForbidden,
			body:       `{"error":{"code":"AuthorizationFailed"}}`,
			operation:  testOperationID,
			expected:   componentSkipped,
			expectHit:  true,
			expectSkip: true,
		},
		{
			name:      "operation id outside hex is rejected before any request",
			status:    http.StatusOK,
			body:      `{}`,
			operation: "x' | drop table",
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits int
			var gotPath, gotQuery string
			svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				gotPath = r.URL.Path
				raw, _ := io.ReadAll(r.Body)
				var body map[string]string
				_ = json.Unmarshal(raw, &body)
				gotQuery = body["query"]
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer svr.Close()

			dsInfo := newLogsTestDsInfo(svr.URL, svr.Client())
			componentID := "/subscriptions/sub-1/resourceGroups/rg/providers/microsoft.insights/components/app-a"
			got, err := componentHasOperation(context.Background(), dsInfo, componentID, tt.operation, testTimeRange())

			if tt.expectErr {
				require.Error(t, err)
				require.Equal(t, 0, hits, "invalid operation id must not reach the server")
				return
			}
			if tt.expectSkip {
				require.Error(t, err, "a skipped component must say why")
				require.Contains(t, err.Error(), "403")
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.expected, got)
			require.Equal(t, 1, hits)
			require.True(t, strings.HasSuffix(gotPath, "/providers/microsoft.insights/components/app-a/query"), "path was %s", gotPath)
			require.Contains(t, gotQuery, fmt.Sprintf("operation_Id == '%s'", tt.operation))
			require.Contains(t, gotQuery, "take 1")
		})
	}
}

// discoveryServer serves a Resource Graph component list and a per-component KQL responder.
// rowsFor maps a component name to the HTTP status and whether the KQL answer has a row.
type discoveryServer struct {
	components []string // resource IDs returned by Resource Graph, unsorted on purpose
	status     map[string]int
	hasRow     map[string]bool
	mu         sync.Mutex
	hits       map[string]int
}

func (d *discoveryServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/providers/Microsoft.ResourceGraph/resources") {
		rows := make([]map[string]string, 0, len(d.components))
		for _, id := range d.components {
			rows = append(rows, map[string]string{"id": id, "subscriptionId": "sub-" + id[len(id)-1:]})
		}
		raw, _ := json.Marshal(map[string]any{"data": rows})
		_, _ = w.Write(raw)
		return
	}
	// Path is .../components/<name>/query.
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	name := parts[len(parts)-2]
	d.hits[name]++
	code := http.StatusOK
	if c, ok := d.status[name]; ok {
		code = c
	}
	if code != http.StatusOK {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"error":{"code":"AuthorizationFailed"}}`)
		return
	}
	rows := "[]"
	if d.hasRow[name] {
		rows = `[["2026-11-18T19:45:00Z"]]`
	}
	_, _ = io.WriteString(w, `{"tables":[{"name":"PrimaryResult","columns":[],"rows":`+rows+`}]}`)
}

func componentID(name string) string {
	return "/subscriptions/sub-" + name[len(name)-1:] + "/resourceGroups/rg/providers/microsoft.insights/components/" + name
}

func newDiscoveryDsInfo(url string, client *http.Client) types.DatasourceInfo {
	return types.DatasourceInfo{
		Services: map[string]types.DatasourceService{
			"Azure Resource Graph": {URL: url, HTTPClient: client},
			"Azure Log Analytics":  {URL: url, HTTPClient: client},
		},
	}
}

func TestDiscoverExemplarComponent(t *testing.T) {
	t.Run("picks the component that has the trace, even when it is not first", func(t *testing.T) {
		d := &discoveryServer{
			components: []string{componentID("app-b"), componentID("app-a")},
			hasRow:     map[string]bool{"app-a": true},
			hits:       map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		got, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.NoError(t, err)
		require.Equal(t, componentID("app-a"), got)
		require.Equal(t, 1, d.hits["app-b"], "app-b shares the first batch and must be checked")
	})

	t.Run("skips a component that returns 403 and uses the next match", func(t *testing.T) {
		d := &discoveryServer{
			components: []string{componentID("app-a"), componentID("app-b")},
			status:     map[string]int{"app-a": http.StatusForbidden},
			hasRow:     map[string]bool{"app-b": true},
			hits:       map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		got, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.NoError(t, err)
		require.Equal(t, componentID("app-b"), got)
	})

	t.Run("no match reports checked components and skipped count", func(t *testing.T) {
		d := &discoveryServer{
			components: []string{componentID("app-a"), componentID("app-b")},
			hits:       map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		_, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.Error(t, err)
		require.Contains(t, err.Error(), "2 Application Insights components")
		require.Contains(t, err.Error(), "skipped 0")
		require.Contains(t, err.Error(), "app-a")
		require.Contains(t, err.Error(), "app-b")
		require.NotContains(t, err.Error(), "first 25")
	})

	t.Run("truncated list says so in the error", func(t *testing.T) {
		var ids []string
		hits := map[string]int{}
		for i := 0; i < exemplarComponentCap+1; i++ {
			ids = append(ids, componentID(fmt.Sprintf("app-%02d", i)))
		}
		d := &discoveryServer{components: ids, hits: hits}
		svr := httptest.NewServer(d)
		defer svr.Close()

		_, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.Error(t, err)
		require.Contains(t, err.Error(), fmt.Sprintf("first %d", exemplarComponentCap))
		require.Len(t, hits, exemplarComponentCap, "components past the cap must not be checked")
	})
}
