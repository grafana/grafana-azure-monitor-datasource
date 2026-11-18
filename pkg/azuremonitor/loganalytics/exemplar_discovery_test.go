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

	"github.com/stretchr/testify/require"

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
