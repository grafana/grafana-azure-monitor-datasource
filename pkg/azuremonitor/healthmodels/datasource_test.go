package healthmodels

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/stretchr/testify/require"

	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/types"
)

type testProxy struct{}

func (testProxy) Do(rw http.ResponseWriter, req *http.Request, client *http.Client) (http.ResponseWriter, error) {
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			log.DefaultLogger.Warn("Failed to close response body", "err", err)
		}
	}()
	rw.WriteHeader(res.StatusCode)
	return rw, nil
}

func TestResourceRequestRejectsRedirectsWithoutChangingClient(t *testing.T) {
	redirects := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/redirected" {
			redirects++
		}
		http.Redirect(w, req, "/redirected", http.StatusFound)
	}))
	defer server.Close()
	client := server.Client()
	req := httptest.NewRequest(http.MethodGet, server.URL, nil)
	req.RequestURI = ""
	rw := httptest.NewRecorder()
	_, err := (&Datasource{Proxy: testProxy{}}).ResourceRequest(rw, req, client)
	require.NoError(t, err)
	require.Equal(t, http.StatusFound, rw.Code)
	require.Zero(t, redirects)
	require.Nil(t, client.CheckRedirect)
}

func TestServerSideQueriesReportUnsupported(t *testing.T) {
	response, err := (&Datasource{}).ExecuteTimeSeriesQuery(context.Background(),
		[]backend.DataQuery{{RefID: "A"}, {RefID: "B"}}, types.DatasourceInfo{}, nil, "", true)
	require.NoError(t, err)
	for _, ref := range []string{"A", "B"} {
		require.ErrorContains(t, response.Responses[ref].Error, "Grafana alert rules are not supported")
		require.Equal(t, backend.ErrorSourceDownstream, response.Responses[ref].ErrorSource)
		require.Empty(t, response.Responses[ref].Frames)
	}
}
