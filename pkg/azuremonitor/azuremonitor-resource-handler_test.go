package azuremonitor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/healthmodels"
	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/metrics"
	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/types"

	"github.com/stretchr/testify/require"
)

func Test_parseResourcePath(t *testing.T) {
	tests := []struct {
		name           string
		original       string
		expectedTarget string
		Err            require.ErrorAssertionFunc
	}{
		{
			"Path with a subscription",
			"/azuremonitor/subscriptions/44693801",
			"/subscriptions/44693801",
			require.NoError,
		},
		{
			"Malformed path",
			"/subscriptions?44693801",
			"",
			require.Error,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := getTarget(tt.original)
			if target != tt.expectedTarget {
				t.Errorf("Unexpected target %s expecting %s", target, tt.expectedTarget)
			}
			tt.Err(t, err)
		})
	}
}

func Test_proxyRequest(t *testing.T) {
	tests := []struct {
		name string
	}{
		{"forwards headers and body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("foo", "bar")
				_, err := w.Write([]byte("result"))
				if err != nil {
					t.Fatal(err)
				}
			}))
			req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
			if err != nil {
				t.Error(err)
			}
			rw := httptest.NewRecorder()
			proxy := httpServiceProxy{}
			res, err := proxy.Do(rw, req, srv.Client())
			if err != nil {
				t.Error(err)
			}
			if res.Header().Get("foo") != "bar" {
				t.Errorf("Unexpected headers: %v", res.Header())
			}
			result := rw.Result()
			body, err := io.ReadAll(result.Body)
			if err != nil {
				t.Error(err)
			}
			err = result.Body.Close()
			if err != nil {
				t.Error(err)
			}
			if string(body) != "result" {
				t.Errorf("Unexpected body: %v", string(body))
			}
		})
	}
}

type fakeProxy struct {
	requestedURL    string
	requestedClient *http.Client
}

func (s *fakeProxy) Do(rw http.ResponseWriter, req *http.Request, cli *http.Client) (http.ResponseWriter, error) {
	s.requestedURL = req.URL.String()
	s.requestedClient = cli
	return nil, nil
}

func TestHealthModelsDiscoveryUsesExistingARMResourceProxy(t *testing.T) {
	const modelID = "/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.CloudHealth/healthmodels/model-1"
	proxy := &fakeProxy{}
	armClient := &http.Client{}
	ds := DataSource{
		info: types.DatasourceInfo{
			Services: map[string]types.DatasourceService{
				azureMonitor: {URL: "https://management.azure.com", HTTPClient: armClient},
			},
		},
		executors: map[string]azDatasourceExecutor{
			azureMonitor: &metrics.AzureMonitorDatasource{Proxy: proxy},
		},
		logger: log.DefaultLogger,
	}
	for _, operation := range []string{"", "/entities", "/relationships"} {
		req := httptest.NewRequest(http.MethodGet, "/azuremonitor"+modelID+operation+"?api-version=2026-12-01-preview", nil)
		ds.newResourceMux().ServeHTTP(httptest.NewRecorder(), req)
		require.Equal(t, "https://management.azure.com"+modelID+operation+"?api-version=2026-12-01-preview", proxy.requestedURL)
		require.Same(t, armClient, proxy.requestedClient)
	}
}

func TestHealthModelsResourceUsesConfiguredClientAndSharedProxy(t *testing.T) {
	proxy := &fakeProxy{}
	dataPlaneClient := &http.Client{Transport: http.DefaultTransport}
	ds := DataSource{
		info: types.DatasourceInfo{Services: map[string]types.DatasourceService{
			azureHealthModels: {URL: "https://data.cloudhealth.azure.com", HTTPClient: dataPlaneClient},
		}},
		executors: map[string]azDatasourceExecutor{azureHealthModels: &healthmodels.Datasource{Proxy: proxy}},
		logger:    log.DefaultLogger,
	}
	for _, path := range []string{"api/entities/child-entity", "api/entities/child-entity/transitions"} {
		params := url.Values{
			"dataplaneUrl": {"https://model-1.data.cloudhealth.azure.com"},
			"path":         {path}, "api-version": {healthmodels.DataPlaneAPIVersion}, "timestamp": {"2026-11-24T23:00:00Z"},
		}
		req := httptest.NewRequest(http.MethodGet, "/healthmodels?"+params.Encode(), nil)
		ds.newResourceMux().ServeHTTP(httptest.NewRecorder(), req)
		upstream, err := url.Parse(proxy.requestedURL)
		require.NoError(t, err)
		require.Equal(t, "model-1.data.cloudhealth.azure.com", upstream.Host)
		require.Equal(t, "/"+path, upstream.Path)
		require.Equal(t, healthmodels.DataPlaneAPIVersion, upstream.Query().Get("api-version"))
		require.Same(t, dataPlaneClient.Transport, proxy.requestedClient.Transport)
		require.NotNil(t, proxy.requestedClient.CheckRedirect)
		require.Nil(t, dataPlaneClient.CheckRedirect)
	}
}

func TestHealthModelsResourceRejectsInvalidRequests(t *testing.T) {
	for _, tc := range []struct {
		name       string
		method     string
		endpoint   string
		path       string
		configured bool
		status     int
	}{
		{"writes", http.MethodPost, "https://model-1.data.cloudhealth.azure.com", "api/entities/child-entity", true, http.StatusMethodNotAllowed},
		{"unsafe endpoint", http.MethodGet, "https://attacker.test", "api/entities/child-entity", true, http.StatusBadRequest},
		{"unsupported operation", http.MethodGet, "https://model-1.data.cloudhealth.azure.com", "api/entities/child-entity/delete", true, http.StatusBadRequest},
		{"unconfigured cloud", http.MethodGet, "https://model-1.data.cloudhealth.azure.com", "api/entities/child-entity", false, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := &fakeProxy{}
			ds := DataSource{
				info:      types.DatasourceInfo{Services: map[string]types.DatasourceService{}},
				executors: map[string]azDatasourceExecutor{azureHealthModels: &healthmodels.Datasource{Proxy: proxy}},
				logger:    log.DefaultLogger,
			}
			if tc.configured {
				ds.info.Services[azureHealthModels] = types.DatasourceService{URL: "https://data.cloudhealth.azure.com", HTTPClient: &http.Client{}}
			}
			params := url.Values{"dataplaneUrl": {tc.endpoint}, "path": {tc.path}, "api-version": {healthmodels.DataPlaneAPIVersion}}
			rw := httptest.NewRecorder()
			ds.newResourceMux().ServeHTTP(rw, httptest.NewRequest(tc.method, "/healthmodels?"+params.Encode(), nil))
			require.Equal(t, tc.status, rw.Code)
			require.Contains(t, rw.Body.String(), "error")
			require.Empty(t, proxy.requestedURL)
		})
	}
}

func Test_handleResourceReq(t *testing.T) {
	proxy := &fakeProxy{}
	s := DataSource{
		info: types.DatasourceInfo{
			Services: map[string]types.DatasourceService{
				azureMonitor: {
					URL:        "https://management.azure.com",
					HTTPClient: &http.Client{},
					Logger:     log.DefaultLogger,
				},
			},
		},
		executors: map[string]azDatasourceExecutor{
			azureMonitor: &metrics.AzureMonitorDatasource{
				Proxy: proxy,
			},
		},
		logger: log.DefaultLogger,
	}
	rw := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodGet, "http://foo/azuremonitor/subscriptions/44693801/locations", nil)
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}
	s.handleResourceReq(azureMonitor)(rw, req)
	expectedURL := "https://management.azure.com/subscriptions/44693801/locations"
	if proxy.requestedURL != expectedURL {
		t.Errorf("Unexpected result URL. Got %s, expecting %s", proxy.requestedURL, expectedURL)
	}
}
