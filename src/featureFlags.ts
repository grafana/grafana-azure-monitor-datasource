import { OFREPWebProvider } from '@openfeature/ofrep-web-provider';
import { MultiProvider, OpenFeature, ProviderEvents } from '@openfeature/web-sdk';
import { useEffect, useState } from 'react';

import { config, createOpenFeatureLocalStorageProvider, createOpenFeatureOFREPWebProvider } from '@grafana/runtime';

import pluginJson from './plugin.json';

// OpenFeature is a window-global singleton, so evaluations go through a
// plugin-scoped domain to stay isolated from Grafana core and other plugins.
export const OPEN_FEATURE_DOMAIN = pluginJson.id;

// Registered in pkg/services/featuremgmt/registry.go.
export const BATCH_API_FLAG = 'datasources.azureMonitorBatchAPI';

/**
 * Registers read-only proxies of Grafana's own providers under the plugin's
 * domain. Grafana initializes the underlying providers before plugins load, so
 * the proxies resolve synchronously with no extra flag fetch. Call once at
 * plugin module load.
 */
export function initFeatureFlags(): void {
  // Register when the domain does not already have a provider,
  // so module re-evaluation does not reset OpenFeature state.
  if (OpenFeature.getProvider(OPEN_FEATURE_DOMAIN) === OpenFeature.getProvider()) {
    // Only newer Grafana versions export the shared provider factories;
    // use them if available, or fallback to our own OFREP provider.
    if (
      typeof createOpenFeatureLocalStorageProvider === 'function' &&
      typeof createOpenFeatureOFREPWebProvider === 'function'
    ) {
      OpenFeature.setProvider(
        OPEN_FEATURE_DOMAIN,
        new MultiProvider([
          { provider: createOpenFeatureLocalStorageProvider() },
          { provider: createOpenFeatureOFREPWebProvider() },
        ]),
      );
    } else {
      OpenFeature.setProvider(
        OPEN_FEATURE_DOMAIN,
        new OFREPWebProvider({
          baseUrl: `${config.appSubUrl || ''}/apis/features.grafana.app/v0alpha1/namespaces/${config.namespace}`,
          disableVisibilityRefresh: true, // Do not refresh
          cacheMode: 'disabled', // Do not write to localStorage
          timeoutMs: 10_000, // Timeout after 10 seconds
        }),
        {
          // Standard context used by all plugins
          targetingKey: config.namespace,
          ...config.openFeatureContext,
        },
      );
    }
  }
}

/**
 * Synchronous read of the Metrics Batch API flag. Falls back to the bootstrap
 * `config.featureToggles` value when the provider has no authoritative answer
 * (flag not in the bulk response, provider errored or never initialized —
 * e.g. anonymous sessions, where core skips OFREP initialization).
 */
export function isBatchAPIFlagEnabled(): boolean {
  const details = OpenFeature.getClient(OPEN_FEATURE_DOMAIN).getBooleanDetails(BATCH_API_FLAG, false);
  if (details.errorCode) {
    return Boolean(config.featureToggles[BATCH_API_FLAG]);
  }
  return details.value;
}

/** React hook for the flag; re-renders when the provider (re)initializes. */
export function useBatchAPIFlag(): boolean {
  const [enabled, setEnabled] = useState(isBatchAPIFlagEnabled);

  useEffect(() => {
    const client = OpenFeature.getClient(OPEN_FEATURE_DOMAIN);
    // Provider events fire before the client exposes the new values, so the
    // read is deferred a microtask.
    const update = () => queueMicrotask(() => setEnabled(isBatchAPIFlagEnabled()));
    client.addHandler(ProviderEvents.Ready, update);
    client.addHandler(ProviderEvents.ConfigurationChanged, update);
    // Synchronous re-read in case the provider became ready between render
    // and effect; only event handlers need the microtask deferral.
    setEnabled(isBatchAPIFlagEnabled());
    return () => {
      client.removeHandler(ProviderEvents.Ready, update);
      client.removeHandler(ProviderEvents.ConfigurationChanged, update);
    };
  }, []);

  return enabled;
}
