package azuremonitor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/metrics"
	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/types"
)

// Step-by-step walkthrough of grafana/support-escalations#24106.
//
// This stands in a fake HTTP server for Azure's real ARM API (management.azure.com)
// and drives the exact same handleResourceReq function that Grafana calls when a
// user hits /api/datasources/uid/<uid>/resources/azuremonitor/<path>.
func TestWalkthrough_ResourceProxyForwardsArbitraryMethod(t *testing.T) {
	// 1. This stands in for Azure. It just records whatever request arrives,
	//    and pretends to be the real ARM "listKeys" endpoint for a storage account.
	fakeAzure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf(">> fake Azure received: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"keys":[{"keyName":"key1","value":"THIS-WOULD-BE-A-REAL-STORAGE-KEY"}]}`))
	}))
	defer fakeAzure.Close()

	// 2. This is the DataSource struct exactly as the real plugin builds it in
	//    azuremonitor.go -- pointed at our fake Azure instead of the real one.
	ds := DataSource{
		info: types.DatasourceInfo{
			Services: map[string]types.DatasourceService{
				azureMonitor: {
					URL:        fakeAzure.URL,
					HTTPClient: fakeAzure.Client(),
					Logger:     log.DefaultLogger,
				},
			},
		},
		executors: map[string]azDatasourceExecutor{
			azureMonitor: &metrics.AzureMonitorDatasource{
				Proxy: &httpServiceProxy{logger: log.DefaultLogger},
			},
		},
		logger: log.DefaultLogger,
	}

	// 3. This is the request a Viewer/Editor sends. Note: /listKeys has nothing to
	//    do with metrics or monitoring -- it's an ARM management action. A legitimate
	//    Azure Monitor call never looks like this.
	req, err := http.NewRequest(
		http.MethodPost,
		"http://foo/azuremonitor/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Storage/storageAccounts/acct-1/listKeys?api-version=2022-09-01",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	// 4. This is literally what Grafana core calls for you on the resources route.
	rw := httptest.NewRecorder()
	ds.handleResourceReq(azureMonitor)(rw, req)

	// 5. See what the caller (the Viewer/Editor) got back.
	result := rw.Result()
	body, _ := io.ReadAll(result.Body)
	t.Logf(">> caller received: HTTP %d, body: %s", result.StatusCode, body)
}
