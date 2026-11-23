package loganalytics

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
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
	require.Contains(t, err.Error(), "403 Forbidden")
	require.NotContains(t, err.Error(), "raw azure body")
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
			appID := appIDFor(name)
			if d.noAppID[name] {
				appID = ""
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
	q := buildUnionQuery([]appInsightsComponent{{ID: componentID("app-a"), AppID: appIDFor("app-a")}}, testOperationID)
	appID := appIDFor("app-a")
	require.True(t, strings.HasPrefix(q, fmt.Sprintf("union app('%s').requests, app('%s').dependencies", appID, appID)), q)
	require.Contains(t, q, fmt.Sprintf("app('%s').availabilityResults", appID))
	require.Contains(t, q, fmt.Sprintf("where operation_Id == '%s'", testOperationID))
	require.True(t, strings.HasSuffix(q, "summarize rows = count() by _ResourceId"), q)
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
		require.Contains(t, err.Error(), "in 2 Application Insights components")
		require.NotContains(t, err.Error(), "skipped")
		require.NotContains(t, err.Error(), "first ")
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
		require.Contains(t, err.Error(), "in 1 Application Insights components")
		require.Contains(t, err.Error(), "skipped 1: app-a (subscription sub-a)")
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

	t.Run("a server error stops the lookup instead of retrying", func(t *testing.T) {
		d := &discoveryServer{
			components:  []string{componentID("app-a"), componentID("app-b")},
			unionStatus: http.StatusInternalServerError,
			hits:        map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		_, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.Error(t, err)
		require.Contains(t, err.Error(), "500")
		require.Equal(t, 1, d.unions, "a 500 must not be split or retried")
		require.Empty(t, d.hits["app-a"], "a 500 must not fall back to per-component checks")
	})

	t.Run("throttling stops the lookup instead of splitting the batch", func(t *testing.T) {
		d := &discoveryServer{
			components:  []string{componentID("app-a"), componentID("app-b")},
			unionStatus: http.StatusTooManyRequests,
			hits:        map[string]int{},
		}
		svr := httptest.NewServer(d)
		defer svr.Close()

		_, err := discoverExemplarComponent(context.Background(), newDiscoveryDsInfo(svr.URL, svr.Client()), testOperationID, testTimeRange(), log.DefaultLogger)
		require.Error(t, err)
		require.Contains(t, err.Error(), "429")
		require.Equal(t, 1, d.unions, "throttled requests must not be multiplied")
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
		require.Contains(t, err.Error(), "could not check any of 2 Application Insights components")
		require.NotContains(t, err.Error(), "not found")
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
		require.Contains(t, err.Error(), fmt.Sprintf("first %d components", exemplarComponentCap))
		require.Len(t, hits, exemplarComponentCap, "components past the cap must not be checked")
	})
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
	require.ErrorContains(t, err, "was not found in 60 Application Insights components")
	require.Equal(t, 1, maxInFlight)
	require.Equal(t, (len(ids)+exemplarUnionBatchSize-1)/exemplarUnionBatchSize, d.unions)
}
