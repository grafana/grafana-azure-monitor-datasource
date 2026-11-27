package loganalytics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
	testComponentA  = "/subscriptions/sub-1/resourcegroups/rg/providers/microsoft.insights/components/app-a"
	testComponentB  = "/subscriptions/sub-1/resourcegroups/rg/providers/microsoft.insights/components/app-b"
	testComponentC  = "/subscriptions/sub-2/resourcegroups/rg/providers/microsoft.insights/components/app-c"
	testOperationID = "d958cc15dfc4fbaafeb3f5d8a04e3d01"
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
			_, _ = io.WriteString(w, fmt.Sprintf(`{"data":[{"id":%q,"subscriptionId":"sub-2","appId":"appid-c"}]}`, testComponentC))
			return
		}
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"data":[{"id":%q,"subscriptionId":"sub-1","appId":"appid-b"},{"id":%q,"subscriptionId":"sub-1","appId":"appid-a"}],"$skipToken":"tok"}`,
			testComponentB, testComponentA))
	}))
	defer svr.Close()

	got, truncated, err := listReadableAppInsightsComponents(context.Background(), newARGTestDsInfo(svr.URL, svr.Client()))
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []appInsightsComponent{
		{ID: testComponentA, SubscriptionID: "sub-1", AppID: "appid-a"},
		{ID: testComponentB, SubscriptionID: "sub-1", AppID: "appid-b"},
		{ID: testComponentC, SubscriptionID: "sub-2", AppID: "appid-c"},
	}, got)

	require.Len(t, bodies, 2)
	_, hasSubscriptions := bodies[0]["subscriptions"]
	require.False(t, hasSubscriptions, "first request must not filter by subscription")
	require.Equal(t, "tok", bodies[1]["options"].(map[string]any)["$skipToken"])
	require.Contains(t, bodies[0]["query"], "properties.AppId")
	require.Contains(t, bodies[0]["query"], fmt.Sprintf("take %d", exemplarComponentCap+1))
	require.Equal(t, float64(exemplarComponentCap+1), bodies[0]["options"].(map[string]any)["$top"])
}

func TestListReadableAppInsightsComponents_TruncatesAtCap(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows := make([]string, 0, exemplarComponentCap+1)
		for i := 0; i <= exemplarComponentCap; i++ {
			rows = append(rows, fmt.Sprintf(`{"id":"/subscriptions/s/resourcegroups/rg/providers/microsoft.insights/components/app-%03d","subscriptionId":"s"}`, i))
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

func TestListReadableAppInsightsComponents_Forbidden(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"AccessDenied","message":"raw azure body"}}`)
	}))
	defer svr.Close()

	_, _, err := listReadableAppInsightsComponents(context.Background(), newARGTestDsInfo(svr.URL, svr.Client()))
	require.Error(t, err)
	require.EqualError(t, err, "no Application Insights components are readable by this identity (Azure Resource Graph returned 403 Forbidden)")
}

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
		expectErr  bool
		expectSkip bool
		// expectFailed means the check must stop the lookup, not just skip this component.
		expectFailed bool
		wantErr      string
	}{
		{
			name:      "row returned means the trace is in this component",
			status:    http.StatusOK,
			body:      `{"tables":[{"name":"PrimaryResult","columns":[{"name":"timestamp","type":"datetime"}],"rows":[["2026-11-18T19:45:00Z"]]}]}`,
			operation: testOperationID,
			expected:  componentHasTrace,
		},
		{
			name:      "no rows means the component does not have it",
			status:    http.StatusOK,
			body:      `{"tables":[{"name":"PrimaryResult","columns":[],"rows":[]}]}`,
			operation: testOperationID,
			expected:  componentAbsent,
		},
		{
			name:       "403 skips the component without failing the lookup",
			status:     http.StatusForbidden,
			body:       `{"error":{"code":"AuthorizationFailed"}}`,
			operation:  testOperationID,
			expected:   componentSkipped,
			expectSkip: true,
			wantErr:    `presence check returned 403 Forbidden: {"error":{"code":"AuthorizationFailed"}}`,
		},
		{
			name:         "401 fails the lookup because every component would fail the same way",
			status:       http.StatusUnauthorized,
			body:         `{"error":{"code":"ExpiredAuthenticationToken"}}`,
			wantErr:      `presence check returned 401 Unauthorized: {"error":{"code":"ExpiredAuthenticationToken"}}`,
			operation:    testOperationID,
			expected:     componentFailed,
			expectFailed: true,
		},
		{
			name:         "429 fails the lookup instead of reporting the component as skipped",
			status:       http.StatusTooManyRequests,
			body:         `{"error":{"code":"TooManyRequests"}}`,
			wantErr:      `presence check returned 429 Too Many Requests: {"error":{"code":"TooManyRequests"}}`,
			operation:    testOperationID,
			expected:     componentFailed,
			expectFailed: true,
		},
		{
			name:         "500 fails the lookup",
			status:       http.StatusInternalServerError,
			body:         `{"error":{"code":"InternalServerError"}}`,
			wantErr:      `presence check returned 500 Internal Server Error: {"error":{"code":"InternalServerError"}}`,
			operation:    testOperationID,
			expected:     componentFailed,
			expectFailed: true,
		},
		{
			name:      "operation id outside hex is rejected before any request",
			status:    http.StatusOK,
			body:      `{}`,
			operation: "x' | drop table",
			expectErr: true,
			wantErr:   "invalid operation id for trace exemplar lookup",
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
				require.EqualError(t, err, tt.wantErr)
				require.Equal(t, 0, hits, "invalid operation id must not reach the server")
				return
			}
			if tt.expectSkip || tt.expectFailed {
				require.EqualError(t, err, tt.wantErr)
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

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

func TestComponentHasOperationFailsOnTransportProblems(t *testing.T) {
	componentID := "/subscriptions/sub-1/resourceGroups/rg/providers/microsoft.insights/components/app-a"

	t.Run("a connection error fails the check and names the cause, not the URL", func(t *testing.T) {
		client := &http.Client{Transport: failingTransport{err: &url.Error{
			Op:  "Post",
			URL: "https://api.loganalytics.io/v1" + componentID + "/query",
			Err: errors.New("dial tcp: connection refused"),
		}}}
		dsInfo := newLogsTestDsInfo("https://api.loganalytics.io", client)

		status, err := componentHasOperation(context.Background(), dsInfo, componentID, testOperationID, testTimeRange())
		require.Equal(t, componentFailed, status)
		require.Error(t, err)
		require.EqualError(t, err, "could not reach Azure Log Analytics: dial tcp: connection refused")
	})

	t.Run("a cancelled context fails the check", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"tables":[]}`)
		}))
		defer svr.Close()
		dsInfo := newLogsTestDsInfo(svr.URL, svr.Client())

		status, err := componentHasOperation(ctx, dsInfo, componentID, testOperationID, testTimeRange())
		require.Equal(t, componentFailed, status)
		require.EqualError(t, err, "could not reach Azure Log Analytics: context canceled")
	})

	t.Run("a response cut off mid-body fails the check", func(t *testing.T) {
		svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "500")
			_, _ = io.WriteString(w, `{"tables":[`)
		}))
		defer svr.Close()
		dsInfo := newLogsTestDsInfo(svr.URL, svr.Client())

		status, err := componentHasOperation(context.Background(), dsInfo, componentID, testOperationID, testTimeRange())
		require.Equal(t, componentFailed, status)
		require.EqualError(t, err, "could not read Azure Log Analytics response: unexpected EOF")
	})
}

// discoveryServer serves a Resource Graph component list, union queries, and per-component queries.
//
// Components are named by their last path segment. Each has a GUID app ID derived from its name. status marks
// components that answer with an HTTP error; a 403 also makes a union that names the component fail
// with InsufficientAccessToResource. hasRow marks components that hold the trace.
type discoveryServer struct {
	components []string // resource IDs returned by Resource Graph, unsorted on purpose
	status     map[string]int
	hasRow     map[string]bool
	noAppID    map[string]bool // components whose Resource Graph row has no app ID
	// routeStatus makes union queries routed through a component fail with this HTTP status, as the
	// service does when the route component itself cannot be read.
	routeStatus map[string]int
	// unionStatus, when set, makes every union query fail with this HTTP status and no denial details.
	// 400 is what the service returns for oversized or rejected queries.
	unionStatus int
	// maxUnionApps, when set, makes union queries that name more distinct apps fail with a
	// BadArgumentError that names no resource, as the service does for oversized queries.
	maxUnionApps int
	mu           sync.Mutex
	hits         map[string]int // queries that named each component, union or per-component
	unions       int            // union queries received
}

var unionAppPattern = regexp.MustCompile(`app\('([^']+)'\)`)

// appIDFor returns a GUID-shaped app ID for a component name, as Application Insights returns.
func appIDFor(name string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	sum := h.Sum64()
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", uint32(sum>>32), sum&0xffffffffffff)
}

// nameForAppID returns the component name that owns an app ID, or "" if none does.
func (d *discoveryServer) nameForAppID(appID string) string {
	for _, id := range d.components {
		if name := componentName(id); appIDFor(name) == appID {
			return name
		}
	}
	return ""
}

func componentName(id string) string {
	return id[strings.LastIndex(id, "/")+1:]
}

func (d *discoveryServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/providers/Microsoft.ResourceGraph/resources") {
		rows := make([]map[string]string, 0, len(d.components))
		for _, id := range d.components {
			name := componentName(id)
			var appID string
			if !d.noAppID[name] {
				appID = appIDFor(name)
			}
			rows = append(rows, map[string]string{
				"id":             id,
				"subscriptionId": "sub-" + name[len(name)-1:],
				"appId":          appID,
			})
		}
		raw, _ := json.Marshal(map[string]any{"data": rows})
		_, _ = w.Write(raw)
		return
	}

	raw, _ := io.ReadAll(r.Body)
	var body map[string]string
	_ = json.Unmarshal(raw, &body)

	if strings.Contains(body["query"], "summarize rows") {
		d.unions++
		routeParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if code, ok := d.routeStatus[routeParts[len(routeParts)-2]]; ok {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"error":{"code":"Forbidden"}}`)
			return
		}
		if d.unionStatus != 0 {
			w.WriteHeader(d.unionStatus)
			_, _ = io.WriteString(w, `{"error":{"code":"BadArgumentError","message":"The request had some invalid properties"}}`)
			return
		}
		var names []string
		seen := make(map[string]bool)
		for _, m := range unionAppPattern.FindAllStringSubmatch(body["query"], -1) {
			name := d.nameForAppID(m[1])
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
		if d.maxUnionApps > 0 && len(names) > d.maxUnionApps {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"BadArgumentError","message":"The request had some invalid properties"}}`)
			return
		}
		for _, n := range names {
			d.hits[n]++
		}

		var details []map[string]string
		for _, n := range names {
			if code, ok := d.status[n]; ok && code == http.StatusForbidden {
				details = append(details, map[string]string{
					"code":    insufficientAccessCode,
					"message": "Cannot access with the current credentials",
					"value":   appIDFor(n),
				})
			}
		}
		if len(details) > 0 {
			w.WriteHeader(http.StatusBadRequest)
			raw, _ := json.Marshal(map[string]any{"error": map[string]any{"code": "BadArgumentError", "details": details}})
			_, _ = w.Write(raw)
			return
		}

		var rows [][]any
		for _, n := range names {
			if d.hasRow[n] {
				rows = append(rows, []any{strings.ToLower(componentID(n)), 1})
			}
		}
		raw, _ := json.Marshal(map[string]any{"tables": []any{map[string]any{
			"name":    "PrimaryResult",
			"columns": []map[string]string{{"name": "_ResourceId", "type": "string"}, {"name": "rows", "type": "long"}},
			"rows":    rows,
		}}})
		_, _ = w.Write(raw)
		return
	}

	// Per-component query: .../components/<name>/query.
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

func TestBuildUnionQuery(t *testing.T) {
	appID := appIDFor("app-a")
	q := buildUnionQuery([]appInsightsComponent{{ID: componentID("app-a"), AppID: appID}}, testOperationID)
	// The full expected query, with tables in the order the service sees them.
	want := fmt.Sprintf("union app('%[1]s').requests, app('%[1]s').dependencies | where operation_Id == '%[2]s' | summarize rows = count() by _ResourceId",
		appID, testOperationID)
	require.Equal(t, want, q)
}

func TestChunkComponents(t *testing.T) {
	var components []appInsightsComponent
	for i := 0; i < 250; i++ {
		components = append(components, appInsightsComponent{ID: fmt.Sprintf("c%d", i)})
	}
	batches := chunkComponents(components, exemplarUnionBatchSize)
	require.Len(t, batches, (250+exemplarUnionBatchSize-1)/exemplarUnionBatchSize)
	for _, b := range batches {
		require.LessOrEqual(t, len(b), exemplarUnionBatchSize)
	}
	require.Equal(t, "c249", batches[len(batches)-1][len(batches[len(batches)-1])-1].ID)
	// The batch size keeps every union query within the measured pair limit.
	require.LessOrEqual(t, exemplarUnionBatchSize*len(unionTables), exemplarUnionPairLimit)
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
		require.Equal(t, 1, d.unions, "both components share one union query")
		require.Equal(t, 1, d.hits["app-b"])
	})

	t.Run("drops a component the caller cannot read and retries the rest", func(t *testing.T) {
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
		require.Equal(t, 2, d.unions, "one union with app-a, then one retry without it")
	})

	t.Run("no match reports how many components were checked", func(t *testing.T) {
		d := &discoveryServer{
			components: []string{componentID("app-a"), componentID("app-b")},
			hits:       map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		_, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.Error(t, err)
		require.EqualError(t, err, fmt.Sprintf("trace %s was not found in 2 Application Insights components this identity can read", testOperationID))
	})

	t.Run("skipped components are named in the error", func(t *testing.T) {
		d := &discoveryServer{
			components: []string{componentID("app-a"), componentID("app-b")},
			status:     map[string]int{"app-a": http.StatusForbidden},
			hits:       map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		_, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.Error(t, err)
		require.EqualError(t, err, fmt.Sprintf("trace %s was not found in 1 Application Insights components this identity can read (skipped 1: not readable by this identity: subscription sub-a (app-a))", testOperationID))
	})

	t.Run("the lookup stops at the first batch that has the trace", func(t *testing.T) {
		var ids []string
		for i := 0; i < 150; i++ {
			ids = append(ids, componentID(fmt.Sprintf("app-%03d", i)))
		}
		// app-120 is in the first subscription, so it is in the first batch.
		d := &discoveryServer{
			components: ids,
			hasRow:     map[string]bool{"app-120": true},
			hits:       map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		got, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.NoError(t, err)
		require.Equal(t, componentID("app-120"), got)
		require.Equal(t, 1, d.unions, "no batch after the matching one is queried")
	})

	t.Run("every batch is queried when the trace is in the last one", func(t *testing.T) {
		var ids []string
		for i := 0; i < 150; i++ {
			ids = append(ids, componentID(fmt.Sprintf("app-%03d", i)))
		}
		// app-149 is last in subscription and resource ID order, so it is in the last batch.
		d := &discoveryServer{
			components: ids,
			hasRow:     map[string]bool{"app-149": true},
			hits:       map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		got, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.NoError(t, err)
		require.Equal(t, componentID("app-149"), got)
		wantBatches := (len(ids) + exemplarUnionBatchSize - 1) / exemplarUnionBatchSize
		require.Equal(t, wantBatches, d.unions)
	})

	t.Run("a batch the service rejects is split until it succeeds", func(t *testing.T) {
		var ids []string
		for i := 0; i < exemplarUnionBatchSize; i++ {
			ids = append(ids, componentID(fmt.Sprintf("app-%03d", i)))
		}
		d := &discoveryServer{
			components:   ids,
			hasRow:       map[string]bool{"app-001": true},
			maxUnionApps: 4,
			hits:         map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		got, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.NoError(t, err)
		require.Equal(t, componentID("app-001"), got)
		require.Greater(t, d.unions, 1, "the rejected batch must be split")
	})

	t.Run("a single component rejected by every union falls back to its own check", func(t *testing.T) {
		d := &discoveryServer{
			components:  []string{componentID("app-a"), componentID("app-b")},
			hasRow:      map[string]bool{"app-a": true},
			unionStatus: http.StatusBadRequest,
			hits:        map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		got, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.NoError(t, err)
		require.Equal(t, componentID("app-a"), got)
		require.Equal(t, 1, d.hits["app-a"], "app-a is checked once on its own")
	})

	t.Run("a component whose route cannot be read is skipped, not fatal", func(t *testing.T) {
		d := &discoveryServer{
			components:  []string{componentID("app-a"), componentID("app-b")},
			routeStatus: map[string]int{"app-a": http.StatusForbidden},
			hasRow:      map[string]bool{"app-b": true},
			hits:        map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		got, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.NoError(t, err)
		require.Equal(t, componentID("app-b"), got)
		require.Equal(t, 2, d.unions, "the retry is routed through app-b")
	})

	t.Run("a skipped route component is named when the trace is not found", func(t *testing.T) {
		d := &discoveryServer{
			components:  []string{componentID("app-a"), componentID("app-b")},
			routeStatus: map[string]int{"app-a": http.StatusNotFound},
			hits:        map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		_, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.Error(t, err)
		require.EqualError(t, err, fmt.Sprintf("trace %s was not found in 1 Application Insights components this identity can read (skipped 1: route component returned 404: subscription sub-a (app-a))", testOperationID))
	})

	t.Run("repeated rejections stop at the query budget", func(t *testing.T) {
		var ids []string
		for i := 0; i < 150; i++ {
			ids = append(ids, componentID(fmt.Sprintf("app-%03d", i)))
		}
		d := &discoveryServer{
			components:  ids,
			unionStatus: http.StatusBadRequest,
			hits:        map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		_, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.EqualError(t, err, fmt.Sprintf("trace exemplar lookup stopped: reached the limit of %d Log Analytics queries for one lookup", exemplarMaxQueries))
		sent := d.unions
		for _, n := range d.hits {
			sent += n
		}
		require.Equal(t, exemplarMaxQueries, sent)
	})

	t.Run("a missing or invalid operation ID is a downstream error", func(t *testing.T) {
		d := &discoveryServer{components: []string{componentID("app-a")}, hits: map[string]int{}}
		svr := httptest.NewServer(d)
		defer svr.Close()
		dsInfo := newDiscoveryDsInfo(svr.URL, svr.Client())

		_, err := discoverExemplarComponent(context.Background(), dsInfo, "", testTimeRange(), log.DefaultLogger)
		require.EqualError(t, err, "trace exemplar lookup needs an operation ID")
		require.True(t, backend.IsDownstreamError(err))

		_, err = discoverExemplarComponent(context.Background(), dsInfo, "not-hex", testTimeRange(), log.DefaultLogger)
		require.EqualError(t, err, "operation ID for trace exemplar lookup must be 1 to 64 hex characters")
		require.True(t, backend.IsDownstreamError(err))
		require.Zero(t, d.unions)
	})

	t.Run("service-wide union failures stop the lookup instead of splitting or retrying", func(t *testing.T) {
		tests := []struct {
			name    string
			status  int
			wantErr string
		}{
			{name: "authentication", status: http.StatusUnauthorized, wantErr: `trace exemplar lookup stopped: union presence check returned 401 Unauthorized: {"error":{"code":"BadArgumentError","message":"The request had some invalid properties"}}`},
			{name: "throttling", status: http.StatusTooManyRequests, wantErr: `trace exemplar lookup stopped: union presence check returned 429 Too Many Requests: {"error":{"code":"BadArgumentError","message":"The request had some invalid properties"}}`},
			{name: "server error", status: http.StatusInternalServerError, wantErr: `trace exemplar lookup stopped: union presence check returned 500 Internal Server Error: {"error":{"code":"BadArgumentError","message":"The request had some invalid properties"}}`},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				d := &discoveryServer{
					components:  []string{componentID("app-a"), componentID("app-b")},
					unionStatus: tt.status,
					hits:        map[string]int{},
				}
				svr := httptest.NewServer(d)
				defer svr.Close()

				_, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
				require.EqualError(t, err, tt.wantErr)
				require.Equal(t, 1, d.unions, "the failure must not be split or retried")
				require.Empty(t, d.hits["app-a"], "the failure must not fall back to per-component checks")
			})
		}
	})

	t.Run("when every component is unreadable the lookup says it could not run", func(t *testing.T) {
		d := &discoveryServer{
			components: []string{componentID("app-a"), componentID("app-b")},
			status:     map[string]int{"app-a": http.StatusForbidden, "app-b": http.StatusForbidden},
			hits:       map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		_, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.Error(t, err)
		require.EqualError(t, err, fmt.Sprintf("could not check any of 2 Application Insights components for trace %s: not readable by this identity: subscription sub-a (app-a), subscription sub-b (app-b)", testOperationID))
	})

	t.Run("a component without an app ID is checked on its own", func(t *testing.T) {
		d := &discoveryServer{
			components: []string{componentID("app-a"), componentID("app-b")},
			noAppID:    map[string]bool{"app-a": true},
			hasRow:     map[string]bool{"app-a": true},
			hits:       map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		got, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.NoError(t, err)
		require.Equal(t, componentID("app-a"), got)
		require.Equal(t, 1, d.hits["app-a"], "app-a is checked on its own, not in a union")
	})

	t.Run("a union match is returned without checking a component on its own", func(t *testing.T) {
		d := &discoveryServer{
			components: []string{componentID("app-a"), componentID("app-b")},
			noAppID:    map[string]bool{"app-a": true},
			status:     map[string]int{"app-a": http.StatusTooManyRequests},
			hasRow:     map[string]bool{"app-b": true},
			hits:       map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		got, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.NoError(t, err)
		require.Equal(t, componentID("app-b"), got)
		require.Equal(t, 0, d.hits["app-a"], "app-a is not checked on its own once the union finds the trace")
	})

	t.Run("throttling on a component checked on its own stops the lookup", func(t *testing.T) {
		d := &discoveryServer{
			components: []string{componentID("app-a"), componentID("app-b")},
			noAppID:    map[string]bool{"app-a": true},
			status:     map[string]int{"app-a": http.StatusTooManyRequests},
			hits:       map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		_, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.Error(t, err)
		require.EqualError(t, err, `trace exemplar lookup stopped: presence check returned 429 Too Many Requests: {"error":{"code":"AuthorizationFailed"}}`)
		require.Equal(t, 1, d.unions, "the union runs first and finds nothing")
	})

	t.Run("truncated list says so and checks only the cap", func(t *testing.T) {
		var ids []string
		hits := map[string]int{}
		for i := 0; i < exemplarComponentCap+1; i++ {
			ids = append(ids, componentID(fmt.Sprintf("app-%03d", i)))
		}
		d := &discoveryServer{components: ids, hits: hits}
		svr := httptest.NewServer(d)
		defer svr.Close()

		_, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.Error(t, err)
		require.EqualError(t, err, fmt.Sprintf("trace %s was not found in %d Application Insights components this identity can read; only the first %d components ordered by subscription and resource id were checked", testOperationID, exemplarComponentCap, exemplarComponentCap))
		require.Len(t, hits, exemplarComponentCap, "components past the cap must not be checked")
	})
}

func TestSkippedList(t *testing.T) {
	subscriptions := map[string]string{
		componentID("app-a"): "sub-a",
		componentID("app-b"): "sub-a",
		componentID("app-c"): "sub-c",
		componentID("app-d"): "sub-b",
	}
	skipped := map[string]string{
		componentID("app-b"): "not readable by this identity",
		componentID("app-a"): "not readable by this identity",
		componentID("app-d"): "not readable by this identity",
		componentID("app-c"): "route component returned 404",
	}
	require.Equal(t,
		"not readable by this identity: subscription sub-a (app-a, app-b), subscription sub-b (app-d); route component returned 404: subscription sub-c (app-c)",
		skippedList(skipped, subscriptions))
}

// TestDiscoverExemplarComponentSendsOneQueryAtATime checks that a lookup never has more than one
// Log Analytics query in flight, so it adds at most one request to Azure's per-user concurrency.
func TestDiscoverExemplarComponentSendsOneQueryAtATime(t *testing.T) {
	var ids []string
	for i := 0; i < 60; i++ {
		ids = append(ids, componentID(fmt.Sprintf("app-%03d", i)))
	}
	d := &discoveryServer{components: ids, hits: map[string]int{}}

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/query") {
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
		}
		d.ServeHTTP(w, r)
	}))
	defer svr.Close()

	_, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), "0123456789abcdef", testTimeRange(), log.DefaultLogger)
	require.EqualError(t, err, fmt.Sprintf("trace 0123456789abcdef was not found in %d Application Insights components this identity can read", len(ids)))
	require.Equal(t, 1, maxInFlight)
	require.Equal(t, (len(ids)+exemplarUnionBatchSize-1)/exemplarUnionBatchSize, d.unions)
}
