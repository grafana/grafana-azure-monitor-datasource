package loganalytics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

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
		name      string
		status    int
		body      string
		operation string
		expected  componentCheck
		expectHit bool
		expectErr bool
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
			name:      "403 skips the component without failing the lookup",
			status:    http.StatusForbidden,
			body:      `{"error":{"code":"AuthorizationFailed"}}`,
			operation: testOperationID,
			expected:  componentSkipped,
			expectHit: true,
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
			require.NoError(t, err)
			require.Equal(t, tt.expected, got)
			require.Equal(t, 1, hits)
			require.True(t, strings.HasSuffix(gotPath, "/v1/apps/app-a/query"), "path was %s", gotPath)
			require.Contains(t, gotQuery, fmt.Sprintf("operation_Id == '%s'", tt.operation))
			require.Contains(t, gotQuery, "take 1")
		})
	}
}
