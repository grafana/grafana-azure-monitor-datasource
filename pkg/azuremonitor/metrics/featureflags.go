package metrics

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
)

const (
	// defaultFlagCacheTTL applies when Grafana gives no caching advice, so a
	// flag is not fetched over HTTP on every query.
	defaultFlagCacheTTL = time.Minute
	flagEvalTimeout     = 5 * time.Second
)

// flagEvaluator evaluates boolean feature flags from the backend.
//
// When the plugin runs inside Grafana's process (as a core plugin), Grafana has
// registered the global OpenFeature provider and it is used as-is. A standalone
// plugin process has no global provider, so flags are evaluated against the
// OFREP endpoint that Grafana advertises in the per-request config. That call
// is unauthenticated, so only flags marked public in Grafana's flag
// configuration resolve; anything else falls back to the default.
type flagEvaluator struct {
	mu        sync.Mutex
	providers map[string]*ofrep.Provider
	cache     map[flagCacheKey]flagCacheEntry
	now       func() time.Time
}

type flagCacheKey struct {
	url          string
	targetingKey string
	flag         string
}

type flagCacheEntry struct {
	value     bool
	expiresAt time.Time
}

func newFlagEvaluator() *flagEvaluator {
	return &flagEvaluator{
		providers: map[string]*ofrep.Provider{},
		cache:     map[flagCacheKey]flagCacheEntry{},
		now:       time.Now,
	}
}

var defaultFlagEvaluator = newFlagEvaluator()

func (f *flagEvaluator) Boolean(ctx context.Context, logger log.Logger, flag string, defaultValue bool) bool {
	if openfeature.ProviderMetadata().Name != (openfeature.NoopProvider{}).Metadata().Name {
		return openfeature.NewDefaultClient().Boolean(ctx, flag, defaultValue, openfeature.TransactionContext(ctx))
	}

	cfg, err := backend.GrafanaConfigFromContext(ctx).OpenFeature()
	if err != nil {
		if !errors.Is(err, config.ErrOpenFeatureNotConfigured) {
			logger.Warn("Invalid OpenFeature config from Grafana; using the flag default", "flag", flag, "error", err)
		} else {
			logger.Debug("Grafana did not advertise an OpenFeature provider; using the flag default", "flag", flag)
		}
		return defaultValue
	}

	// The SDK documents the plugin context namespace as the targeting key, with
	// the host-asserted namespace attribute as the fallback.
	targetingKey := backend.PluginConfigFromContext(ctx).Namespace
	if targetingKey == "" {
		targetingKey = cfg.ContextAttrs["namespace"]
	}

	key := flagCacheKey{url: cfg.URL, targetingKey: targetingKey, flag: flag}
	f.mu.Lock()
	if entry, ok := f.cache[key]; ok && f.now().Before(entry.expiresAt) {
		f.mu.Unlock()
		return entry.value
	}
	provider, ok := f.providers[cfg.URL]
	if !ok {
		provider = ofrep.NewProvider(cfg.URL, ofrep.WithTimeout(flagEvalTimeout))
		f.providers[cfg.URL] = provider
	}
	f.mu.Unlock()

	evalCtx := openfeature.FlattenedContext{}
	for k, v := range cfg.ContextAttrs {
		evalCtx[k] = v
	}
	evalCtx[openfeature.TargetingKey] = targetingKey

	value := defaultValue
	res := provider.BooleanEvaluation(ctx, flag, defaultValue, evalCtx)
	if err := res.Error(); err != nil {
		// Cached like a successful result so an unreachable endpoint does not
		// add a timeout to every query.
		logger.Warn("Failed to evaluate feature flag; using the flag default", "flag", flag, "providerType", cfg.ProviderType, "error", err)
	} else {
		value = res.Value
	}

	ttl := cfg.CacheTTL
	if ttl == 0 {
		ttl = defaultFlagCacheTTL
	}
	f.mu.Lock()
	f.cache[key] = flagCacheEntry{value: value, expiresAt: f.now().Add(ttl)}
	f.mu.Unlock()
	return value
}
