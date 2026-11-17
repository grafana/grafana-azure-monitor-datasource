package metrics

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/kinds/dataquery"
)

// ofrepServer is a fake OFREP endpoint that serves batchAPIFlag and records the
// evaluation context of each request.
type ofrepServer struct {
	*httptest.Server
	mu       sync.Mutex
	status   int
	value    bool
	contexts []map[string]any
}

func newOFREPServer(t *testing.T, status int, value bool) *ofrepServer {
	t.Helper()
	s := &ofrepServer{status: status, value: value}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ofrep/v1/evaluate/flags/"+batchAPIFlag {
			// Anything else is a metrics batch request from the end-to-end test.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(batchSuccessResponse([]string{"/subscriptions/sub-123/resourcegroups/rg/providers/microsoft.compute/virtualmachines/vm1"}, 42.0))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Context map[string]any `json:"context"`
		}
		_ = json.Unmarshal(body, &req)
		s.mu.Lock()
		s.contexts = append(s.contexts, req.Context)
		status := s.status
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status != http.StatusOK {
			_, _ = w.Write([]byte(`{"key":"` + batchAPIFlag + `","errorCode":"FLAG_NOT_FOUND"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"key": batchAPIFlag, "value": s.value, "reason": "STATIC", "variant": "on"})
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *ofrepServer) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.contexts)
}

// ofrepContext returns a request context carrying the OpenFeature discovery
// config Grafana sends to standalone plugins.
func ofrepContext(url, namespace string, extra map[string]string) context.Context {
	cfg := map[string]string{
		config.OpenFeatureProviderURL:  url,
		config.OpenFeatureProviderType: "static",
	}
	for k, v := range extra {
		cfg[k] = v
	}
	ctx := backend.WithGrafanaConfig(context.Background(), backend.NewGrafanaCfg(cfg))
	return backend.WithPluginContext(ctx, backend.PluginContext{Namespace: namespace})
}

// lockNoopProvider holds the OpenFeature test mutex so the global provider stays
// the default NoopProvider, as in a standalone plugin process.
func lockNoopProvider(t *testing.T) {
	t.Helper()
	openfeatureTestMutex.Lock()
	t.Cleanup(openfeatureTestMutex.Unlock)
}

func TestFlagEvaluator(t *testing.T) {
	t.Run("no global provider and no OFREP discovery defaults to off", func(t *testing.T) {
		lockNoopProvider(t)
		assert.False(t, newFlagEvaluator().Boolean(context.Background(), log.DefaultLogger, batchAPIFlag, false))
	})

	t.Run("a registered global provider is used as-is", func(t *testing.T) {
		ctx := stubBatchFlag(t, true)
		assert.True(t, newFlagEvaluator().Boolean(ctx, log.DefaultLogger, batchAPIFlag, false))
	})

	t.Run("evaluates against the advertised OFREP endpoint with the host context", func(t *testing.T) {
		lockNoopProvider(t)
		srv := newOFREPServer(t, http.StatusOK, true)
		ctx := ofrepContext(srv.URL, "stacks-123", map[string]string{config.OpenFeatureContext: `{"namespace":"stacks-123","slug":"mystack"}`})

		assert.True(t, newFlagEvaluator().Boolean(ctx, log.DefaultLogger, batchAPIFlag, false))
		require.Equal(t, 1, srv.requests())
		assert.Equal(t, "stacks-123", srv.contexts[0]["targetingKey"])
		assert.Equal(t, "mystack", srv.contexts[0]["slug"])
	})

	t.Run("falls back to the namespace attribute for the targeting key", func(t *testing.T) {
		lockNoopProvider(t)
		srv := newOFREPServer(t, http.StatusOK, true)
		ctx := ofrepContext(srv.URL, "", map[string]string{config.OpenFeatureContext: `{"namespace":"stacks-456"}`})

		assert.True(t, newFlagEvaluator().Boolean(ctx, log.DefaultLogger, batchAPIFlag, false))
		require.Equal(t, 1, srv.requests())
		assert.Equal(t, "stacks-456", srv.contexts[0]["targetingKey"])
	})

	t.Run("caches results for the advertised TTL", func(t *testing.T) {
		lockNoopProvider(t)
		srv := newOFREPServer(t, http.StatusOK, true)
		ctx := ofrepContext(srv.URL, "stacks-123", map[string]string{config.OpenFeatureCacheTTL: "30"})
		f := newFlagEvaluator()
		now := time.Now()
		f.now = func() time.Time { return now }

		assert.True(t, f.Boolean(ctx, log.DefaultLogger, batchAPIFlag, false))
		assert.True(t, f.Boolean(ctx, log.DefaultLogger, batchAPIFlag, false))
		assert.Equal(t, 1, srv.requests(), "a second evaluation within the TTL is served from cache")

		now = now.Add(31 * time.Second)
		assert.True(t, f.Boolean(ctx, log.DefaultLogger, batchAPIFlag, false))
		assert.Equal(t, 2, srv.requests(), "an expired entry is fetched again")
	})

	t.Run("an evaluation error uses the default and is cached", func(t *testing.T) {
		lockNoopProvider(t)
		srv := newOFREPServer(t, http.StatusNotFound, true)
		ctx := ofrepContext(srv.URL, "stacks-123", nil)
		f := newFlagEvaluator()

		assert.False(t, f.Boolean(ctx, log.DefaultLogger, batchAPIFlag, false))
		assert.False(t, f.Boolean(ctx, log.DefaultLogger, batchAPIFlag, false))
		assert.Equal(t, 1, srv.requests())
	})

	t.Run("malformed discovery uses the default", func(t *testing.T) {
		lockNoopProvider(t)
		ctx := backend.WithGrafanaConfig(context.Background(), backend.NewGrafanaCfg(map[string]string{
			config.OpenFeatureProviderURL: "http://localhost:3000", // no provider type
		}))
		assert.False(t, newFlagEvaluator().Boolean(ctx, log.DefaultLogger, batchAPIFlag, false))
	})
}

// TestExecuteTimeSeriesQueryBatchFlagViaOFREP is the regression test for the
// standalone plugin: with no global OpenFeature provider, a flag that Grafana
// serves over OFREP must route queries to the batch API.
func TestExecuteTimeSeriesQueryBatchFlagViaOFREP(t *testing.T) {
	lockNoopProvider(t)
	srv := newOFREPServer(t, http.StatusOK, true)
	ds := &AzureMonitorDatasource{Logger: log.DefaultLogger}

	q := makeBatchQuery("A", "sub-123", "eastus", []dataquery.AzureMonitorResource{
		{Subscription: strPtr("sub-123"), ResourceGroup: strPtr("rg"), ResourceName: strPtr("vm1"), Region: strPtr("eastus")},
	})
	resp, err := ds.ExecuteTimeSeriesQuery(ofrepContext(srv.URL, "stacks-123", nil), []backend.DataQuery{q}, makeBatchDsInfo(srv.Server), &http.Client{}, "", false)
	require.NoError(t, err)

	dr := resp.Responses["A"]
	require.NoError(t, dr.Error)
	require.Len(t, dr.Frames, 1, "the query should be served by the batch API")
	assert.Equal(t, 1, srv.requests())
}
