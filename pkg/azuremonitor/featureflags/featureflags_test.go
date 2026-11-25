package featureflags

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

const testFlag = "grafana-azure-monitor-datasource.test-flag"

// ofrepServer serves single-flag OFREP evaluations with the given status and
// body, counting requests and keeping the last request body.
type ofrepServer struct {
	*httptest.Server
	requests atomic.Int64
	lastPath atomic.Value
	lastBody atomic.Value
}

func newOFREPServer(t *testing.T, status int, body map[string]any) *ofrepServer {
	t.Helper()
	s := &ofrepServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		s.lastPath.Store(r.URL.Path)
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			s.lastBody.Store(req)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func flagValue(flag string, value bool) map[string]any {
	return map[string]any{"key": flag, "value": value, "reason": "STATIC", "variant": "default"}
}

func flagNotFound(flag string) map[string]any {
	return map[string]any{"key": flag, "errorCode": "FLAG_NOT_FOUND", "errorDetails": "flag not found"}
}

// requestContext returns a request context whose Grafana configuration
// advertises an OpenFeature endpoint of the given type, as a host would.
func requestContext(endpoint, providerType, namespace string, extra map[string]string) context.Context {
	cfg := map[string]string{}
	if endpoint != "" {
		cfg[config.OpenFeatureProviderURL] = endpoint
	}
	if providerType != "" {
		cfg[config.OpenFeatureProviderType] = providerType
	}
	for k, v := range extra {
		cfg[k] = v
	}
	ctx := config.WithGrafanaConfig(context.Background(), config.NewGrafanaCfg(cfg))
	return backend.WithPluginContext(ctx, backend.PluginContext{Namespace: namespace})
}

func TestIsEnabled(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         map[string]any
		defaultValue bool
		want         bool
	}{
		{name: "enabled flag", status: http.StatusOK, body: flagValue(testFlag, true), defaultValue: false, want: true},
		{name: "disabled flag", status: http.StatusOK, body: flagValue(testFlag, false), defaultValue: true, want: false},
		{name: "undefined flag returns a false default", status: http.StatusNotFound, body: flagNotFound(testFlag), defaultValue: false, want: false},
		{name: "undefined flag returns a true default", status: http.StatusNotFound, body: flagNotFound(testFlag), defaultValue: true, want: true},
		{name: "server error returns the default", status: http.StatusInternalServerError, body: map[string]any{"errorDetails": "boom"}, defaultValue: true, want: true},
		{name: "disabled flag in the provider returns the default", status: http.StatusOK, body: map[string]any{"key": testFlag, "value": false, "reason": "DISABLED"}, defaultValue: true, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newOFREPServer(t, tt.status, tt.body)
			ctx := requestContext(srv.URL, "ofrep", "stacks-1", nil)

			require.Equal(t, tt.want, newClient().isEnabled(ctx, testFlag, tt.defaultValue))
			require.Equal(t, int64(1), srv.requests.Load())
		})
	}
}

func TestIsEnabledUnreachableEndpointReturnsDefault(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()

	ctx := requestContext(srv.URL, "ofrep", "stacks-1", nil)
	require.True(t, newClient().isEnabled(ctx, testFlag, true))
	require.False(t, newClient().isEnabled(ctx, testFlag, false))
}

func TestIsEnabledWithoutUsableEndpointReturnsDefault(t *testing.T) {
	srv := newOFREPServer(t, http.StatusOK, flagValue(testFlag, true))

	tests := []struct {
		name string
		ctx  context.Context
	}{
		{name: "no Grafana configuration", ctx: context.Background()},
		{name: "host advertises nothing", ctx: requestContext("", "", "stacks-1", nil)},
		{name: "URL without a provider type", ctx: requestContext(srv.URL, "", "stacks-1", nil)},
		{name: "malformed cache TTL", ctx: requestContext(srv.URL, "ofrep", "stacks-1", map[string]string{config.OpenFeatureCacheTTL: "soon"})},
		{name: "features-service endpoint", ctx: requestContext(srv.URL, "features-service", "stacks-1", nil)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.False(t, newClient().isEnabled(tt.ctx, testFlag, false))
		})
	}
	require.Equal(t, int64(0), srv.requests.Load(), "no evaluation may reach an endpoint the host did not advertise as usable")
}

func TestIsEnabledEvaluatesAgainstStaticEndpointPath(t *testing.T) {
	srv := newOFREPServer(t, http.StatusOK, flagValue(testFlag, true))

	ctx := requestContext(srv.URL+"/grafana/", "static", "default", nil)
	require.True(t, newClient().isEnabled(ctx, testFlag, false))
	require.Equal(t, "/grafana/ofrep/v1/evaluate/flags/"+testFlag, srv.lastPath.Load())
}

func TestIsEnabledCachesResults(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   map[string]any
	}{
		{name: "successful evaluations", status: http.StatusOK, body: flagValue(testFlag, true)},
		{name: "failed evaluations", status: http.StatusNotFound, body: flagNotFound(testFlag)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newOFREPServer(t, tt.status, tt.body)
			ctx := requestContext(srv.URL, "ofrep", "stacks-1", nil)
			c := newClient()

			first := c.isEnabled(ctx, testFlag, true)
			second := c.isEnabled(ctx, testFlag, true)
			require.Equal(t, first, second)
			require.Equal(t, int64(1), srv.requests.Load())
		})
	}
}

func TestIsEnabledCachedFailureReturnsEachCallersDefault(t *testing.T) {
	srv := newOFREPServer(t, http.StatusNotFound, flagNotFound(testFlag))
	ctx := requestContext(srv.URL, "ofrep", "stacks-1", nil)
	c := newClient()

	require.False(t, c.isEnabled(ctx, testFlag, false))
	require.True(t, c.isEnabled(ctx, testFlag, true))
	require.Equal(t, int64(1), srv.requests.Load())
}

func TestIsEnabledCachesPerTenantAndEndpoint(t *testing.T) {
	first := newOFREPServer(t, http.StatusOK, flagValue(testFlag, true))
	second := newOFREPServer(t, http.StatusOK, flagValue(testFlag, false))
	c := newClient()

	require.True(t, c.isEnabled(requestContext(first.URL, "ofrep", "stacks-1", nil), testFlag, false))
	require.True(t, c.isEnabled(requestContext(first.URL, "ofrep", "stacks-2", nil), testFlag, false))
	require.False(t, c.isEnabled(requestContext(second.URL, "static", "stacks-1", nil), testFlag, true))
	require.True(t, c.isEnabled(requestContext(first.URL, "ofrep", "stacks-1", nil), testFlag, false))

	require.Equal(t, int64(2), first.requests.Load())
	require.Equal(t, int64(1), second.requests.Load())
}

func TestIsEnabledCacheExpiresAfterAdvertisedTTL(t *testing.T) {
	srv := newOFREPServer(t, http.StatusOK, flagValue(testFlag, true))
	ctx := requestContext(srv.URL, "ofrep", "stacks-1", map[string]string{config.OpenFeatureCacheTTL: "1"})
	c := newClient()

	c.isEnabled(ctx, testFlag, false)
	require.Equal(t, int64(1), srv.requests.Load())
	require.Eventually(t, func() bool {
		c.isEnabled(ctx, testFlag, false)
		return srv.requests.Load() == 2
	}, 3*time.Second, 50*time.Millisecond)
}

func TestIsEnabledFailedRefreshKeepsPreviousAnswer(t *testing.T) {
	var requests atomic.Int64
	var failing atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"errorDetails": "boom"})
			return
		}
		_ = json.NewEncoder(w).Encode(flagValue(testFlag, false))
	}))
	t.Cleanup(srv.Close)
	ctx := requestContext(srv.URL, "ofrep", "stacks-1", map[string]string{config.OpenFeatureCacheTTL: "1"})
	c := newClient()

	require.False(t, c.isEnabled(ctx, testFlag, true))
	failing.Store(true)
	require.Eventually(t, func() bool {
		c.isEnabled(ctx, testFlag, true)
		return requests.Load() == 2
	}, 3*time.Second, 50*time.Millisecond)
	require.False(t, c.isEnabled(ctx, testFlag, true), "a failed refresh must not replace the host's last answer with the default")
}

func TestEvaluateReusesEntryCachedByAnotherCaller(t *testing.T) {
	srv := newOFREPServer(t, http.StatusOK, flagValue(testFlag, true))
	ctx := requestContext(srv.URL, "ofrep", "stacks-1", nil)
	c := newClient()
	require.True(t, c.isEnabled(ctx, testFlag, false))

	// A caller that read a miss before the first evaluation finished reaches evaluate with a fresh entry in the cache.
	of, err := config.GrafanaConfigFromContext(ctx).OpenFeature()
	require.NoError(t, err)
	evalCtx := evaluationContext(ctx, of.ContextAttrs)
	entry := c.evaluate(ctx, log.DefaultLogger, testFlag+"|"+srv.URL+"|stacks-1", testFlag, of, evalCtx)

	require.True(t, entry.value)
	require.Equal(t, int64(1), srv.requests.Load())
}

func TestCacheTTL(t *testing.T) {
	require.Equal(t, defaultCacheTTL, cacheTTL(config.OpenFeatureConfig{}))
	require.Equal(t, 30*time.Second, cacheTTL(config.OpenFeatureConfig{CacheTTL: 30 * time.Second}))
}

func TestIsEnabledCanceledCallerDoesNotPoisonCache(t *testing.T) {
	srv := newOFREPServer(t, http.StatusOK, flagValue(testFlag, true))
	ctx := requestContext(srv.URL, "ofrep", "stacks-1", nil)
	canceled, cancel := context.WithCancelCause(ctx)
	cancel(errors.New("caller abandoned the query"))
	c := newClient()

	require.True(t, c.isEnabled(canceled, testFlag, false))
	require.True(t, c.isEnabled(ctx, testFlag, false))
	require.Equal(t, int64(1), srv.requests.Load())
}

func TestIsEnabledCollapsesConcurrentLookups(t *testing.T) {
	var requests atomic.Int64
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(flagValue(testFlag, true))
	}))
	t.Cleanup(srv.Close)
	ctx := requestContext(srv.URL, "ofrep", "stacks-1", nil)
	c := newClient()

	const callers = 8
	results := make(chan bool, callers)
	for range callers {
		go func() { results <- c.isEnabled(ctx, testFlag, false) }()
	}
	require.Eventually(t, func() bool { return requests.Load() == 1 }, time.Second, time.Millisecond)
	close(release)
	for range callers {
		require.True(t, <-results)
	}
	require.Equal(t, int64(1), requests.Load())
}

func TestEvaluationRequestCarriesContext(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		attrs     string
		want      map[string]any
	}{
		{
			name:      "request namespace is the targeting key and host attributes are merged",
			namespace: "stacks-42",
			attrs:     `{"stackId":"42","slug":"mystack","env":"test"}`,
			want: map[string]any{
				"targetingKey": "stacks-42",
				"namespace":    "stacks-42",
				"stackId":      "42",
				"slug":         "mystack",
				"env":          "test",
			},
		},
		{
			name:      "host namespace is the fallback targeting key",
			namespace: "",
			attrs:     `{"namespace":"stacks-7"}`,
			want: map[string]any{
				"targetingKey": "stacks-7",
				"namespace":    "stacks-7",
			},
		},
		{
			name:      "host attributes win over plugin attributes",
			namespace: "stacks-1",
			attrs:     `{"namespace":"stacks-9"}`,
			want: map[string]any{
				"targetingKey": "stacks-1",
				"namespace":    "stacks-9",
			},
		},
		{
			name:      "no namespace sends no targeting key",
			namespace: "",
			attrs:     "",
			want:      map[string]any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newOFREPServer(t, http.StatusOK, flagValue(testFlag, true))
			extra := map[string]string{}
			if tt.attrs != "" {
				extra[config.OpenFeatureContext] = tt.attrs
			}
			newClient().isEnabled(requestContext(srv.URL, "ofrep", tt.namespace, extra), testFlag, false)

			body, ok := srv.lastBody.Load().(map[string]any)
			require.True(t, ok, "the server must receive an OFREP request body")
			got, _ := body["context"].(map[string]any)
			if got == nil {
				got = map[string]any{}
			}
			require.Equal(t, tt.want, got)
		})
	}
}

func TestEvaluationMetrics(t *testing.T) {
	tests := []struct {
		name        string
		flag        string
		status      int
		body        func(flag string) map[string]any
		wantOutcome string
	}{
		{name: "miss then hit", flag: "test.metrics-miss", status: http.StatusOK, body: func(f string) map[string]any { return flagValue(f, true) }, wantOutcome: outcomeMiss},
		{name: "error then hit", flag: "test.metrics-error", status: http.StatusNotFound, body: flagNotFound, wantOutcome: outcomeError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The metrics are globals on the default registry, so each case
			// uses its own flag and asserts deltas, which keeps -count=N green.
			srv := newOFREPServer(t, tt.status, tt.body(tt.flag))
			ctx := requestContext(srv.URL, "ofrep", "stacks-1", nil)
			outcomeBefore := testutil.ToFloat64(evaluationsTotal.WithLabelValues(tt.flag, tt.wantOutcome))
			hitBefore := testutil.ToFloat64(evaluationsTotal.WithLabelValues(tt.flag, outcomeHit))
			samplesBefore := histogramSampleCount(t, tt.flag)
			c := newClient()

			c.isEnabled(ctx, tt.flag, false)
			c.isEnabled(ctx, tt.flag, false)

			require.Equal(t, float64(1), testutil.ToFloat64(evaluationsTotal.WithLabelValues(tt.flag, tt.wantOutcome))-outcomeBefore)
			require.Equal(t, float64(1), testutil.ToFloat64(evaluationsTotal.WithLabelValues(tt.flag, outcomeHit))-hitBefore)
			require.Equal(t, uint64(1), histogramSampleCount(t, tt.flag)-samplesBefore)
		})
	}

	t.Run("no usable endpoint", func(t *testing.T) {
		const flag = "test.metrics-unconfigured"
		before := testutil.ToFloat64(evaluationsTotal.WithLabelValues(flag, outcomeUnconfigured))
		samplesBefore := histogramSampleCount(t, flag)

		newClient().isEnabled(context.Background(), flag, false)

		require.Equal(t, float64(1), testutil.ToFloat64(evaluationsTotal.WithLabelValues(flag, outcomeUnconfigured))-before)
		require.Equal(t, uint64(0), histogramSampleCount(t, flag)-samplesBefore)
	})
}

func histogramSampleCount(t *testing.T, flag string) uint64 {
	t.Helper()
	hist, err := requestDurationSeconds.GetMetricWithLabelValues(flag)
	require.NoError(t, err)
	var m dto.Metric
	require.NoError(t, hist.(prometheus.Metric).Write(&m))
	return m.GetHistogram().GetSampleCount()
}
