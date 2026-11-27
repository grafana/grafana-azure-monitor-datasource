package healthmodels

import (
	"context"
	"fmt"
	"net/http"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/types"
)

type Datasource struct {
	Proxy types.ServiceProxy
}

func (d *Datasource) ResourceRequest(rw http.ResponseWriter, req *http.Request, client *http.Client) (http.ResponseWriter, error) {
	// Keep the injected authenticated client intact and do not follow upstream redirects.
	copy := *client
	copy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return d.Proxy.Do(rw, req, &copy)
}

func (d *Datasource) ExecuteTimeSeriesQuery(_ context.Context, queries []backend.DataQuery, _ types.DatasourceInfo, _ *http.Client, _ string, _ bool) (*backend.QueryDataResponse, error) {
	response := backend.NewQueryDataResponse()
	for _, query := range queries {
		response.Responses[query.RefID] = backend.ErrorResponseWithErrorSource(backend.DownstreamError(
			fmt.Errorf("Health Models queries run in dashboards and Explore; server-side queries and Grafana alert rules are not supported"),
		))
	}
	return response, nil
}
