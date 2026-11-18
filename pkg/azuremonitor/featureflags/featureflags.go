// Package featureflags evaluates boolean feature flags over OFREP against the
// endpoint that the plugin's host advertises in each request's Grafana
// configuration, and returns the caller's default when no usable endpoint is
// advertised or the evaluation fails.
package featureflags

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/config"
	"github.com/open-feature/go-sdk-contrib/providers/ofrep"
	"github.com/open-feature/go-sdk/openfeature"
	"golang.org/x/sync/singleflight"
)

// BatchAPI gates routing of metrics queries to the Azure Monitor metrics batch API.
const BatchAPI = "datasources.azureMonitorBatchAPI"

const (
	requestTimeout  = 5 * time.Second
	defaultCacheTTL = time.Minute
)

type client struct {
	group singleflight.Group

	providersMu sync.Mutex
	providers   map[string]openfeature.FeatureProvider

	cacheMu sync.RWMutex
	cache   map[string]cacheEntry
}

type cacheEntry struct {
	value      bool
	useDefault bool
	expires    time.Time
}

func newClient() *client {
	return &client{
		providers: map[string]openfeature.FeatureProvider{},
		cache:     map[string]cacheEntry{},
	}
}

var defaultClient = newClient()

// IsEnabled evaluates flag for the tenant of ctx and returns defaultValue when the evaluation has no result.
func IsEnabled(ctx context.Context, flag string, defaultValue bool) bool {
	return defaultClient.isEnabled(ctx, flag, defaultValue)
}

func (c *client) isEnabled(ctx context.Context, flag string, defaultValue bool) bool {
	logger := log.DefaultLogger.FromContext(ctx)

	of, err := config.GrafanaConfigFromContext(ctx).OpenFeature()
	if err != nil {
		if !errors.Is(err, config.ErrOpenFeatureNotConfigured) {
			logger.Debug("Invalid OpenFeature configuration from the host", "error", err)
		}
		evaluationsTotal.WithLabelValues(flag, outcomeUnconfigured).Inc()
		return defaultValue
	}
	// Plugins have no credentials for a features-service endpoint.
	if of.ProviderType == "features-service" {
		evaluationsTotal.WithLabelValues(flag, outcomeUnconfigured).Inc()
		return defaultValue
	}

	evalCtx := evaluationContext(ctx, of.ContextAttrs)
	targetingKey, _ := evalCtx[openfeature.TargetingKey].(string)
	key := flag + "|" + of.URL + "|" + targetingKey

	entry, fresh := c.cached(key)
	if fresh {
		evaluationsTotal.WithLabelValues(flag, outcomeHit).Inc()
	} else {
		entry = c.evaluate(ctx, logger, key, flag, of, evalCtx)
	}

	if entry.useDefault {
		return defaultValue
	}
	return entry.value
}

func (c *client) cached(key string) (cacheEntry, bool) {
	c.cacheMu.RLock()
	defer c.cacheMu.RUnlock()
	entry, ok := c.cache[key]
	return entry, ok && time.Now().Before(entry.expires)
}

// evaluate collapses concurrent lookups for key into one OFREP request and caches its answer for the TTL.
// A failed request keeps the previous answer, or the caller's default when there is none.
func (c *client) evaluate(ctx context.Context, logger log.Logger, key, flag string, of config.OpenFeatureConfig, evalCtx openfeature.FlattenedContext) cacheEntry {
	v, _, _ := c.group.Do(key, func() (any, error) {
		previous, fresh := c.cached(key)
		if fresh {
			evaluationsTotal.WithLabelValues(flag, outcomeHit).Inc()
			return previous, nil
		}

		// The result is shared with every caller in the TTL window, so one caller's cancellation must not fail it.
		detached := context.WithoutCancel(ctx)
		start := time.Now()
		detail := c.provider(of.URL).BooleanEvaluation(detached, flag, false, evalCtx)
		requestDurationSeconds.WithLabelValues(flag).Observe(time.Since(start).Seconds())

		entry := cacheEntry{value: detail.Value, useDefault: detail.Reason == openfeature.DisabledReason}
		outcome := outcomeMiss
		if detail.Reason == openfeature.ErrorReason {
			outcome = outcomeError
			logger.Warn("Feature flag evaluation failed", "flag", flag, "error", detail.ResolutionError.Error())
			entry = previous
			if previous.expires.IsZero() {
				entry = cacheEntry{useDefault: true}
			}
		}
		entry.expires = time.Now().Add(cacheTTL(of))
		evaluationsTotal.WithLabelValues(flag, outcome).Inc()

		c.cacheMu.Lock()
		c.cache[key] = entry
		c.cacheMu.Unlock()
		return entry, nil
	})
	return v.(cacheEntry)
}

func (c *client) provider(endpoint string) openfeature.FeatureProvider {
	c.providersMu.Lock()
	defer c.providersMu.Unlock()
	p, ok := c.providers[endpoint]
	if !ok {
		p = ofrep.NewProvider(endpoint, ofrep.WithTimeout(requestTimeout))
		c.providers[endpoint] = p
	}
	return p
}

// evaluationContext targets the request's namespace and adds the host's attributes, which win on a name clash.
func evaluationContext(ctx context.Context, hostAttrs map[string]string) openfeature.FlattenedContext {
	namespace := backend.PluginConfigFromContext(ctx).Namespace
	if namespace == "" {
		namespace = hostAttrs["namespace"]
	}

	evalCtx := make(openfeature.FlattenedContext, len(hostAttrs)+2)
	if namespace != "" {
		evalCtx[openfeature.TargetingKey] = namespace
		evalCtx["namespace"] = namespace
	}
	for k, v := range hostAttrs {
		evalCtx[k] = v
	}
	return evalCtx
}

func cacheTTL(of config.OpenFeatureConfig) time.Duration {
	if of.CacheTTL > 0 {
		return of.CacheTTL
	}
	return defaultCacheTTL
}
