import { OpenFeatureProvider, OpenFeatureTestProvider } from '@openfeature/react-sdk';
import { InMemoryProvider, OpenFeature } from '@openfeature/web-sdk';
import { act, renderHook, waitFor } from '@testing-library/react';

import { HEALTH_MODELS_FLAG, OPEN_FEATURE_DOMAIN, useHealthModelsFlag } from './featureFlags';

const healthModelsFlagConfig = (enabled: boolean) => ({
  [HEALTH_MODELS_FLAG]: {
    variants: { on: true, off: false },
    defaultVariant: enabled ? 'on' : 'off',
    disabled: false,
  },
});

describe('featureFlags', () => {
  afterEach(async () => {
    await OpenFeature.clearProviders();
  });

  describe('useHealthModelsFlag', () => {
    it('re-renders with the flag value when the provider becomes ready', async () => {
      const { result } = renderHook(() => useHealthModelsFlag(), {
        wrapper: ({ children }) => (
          <OpenFeatureTestProvider
            domain={OPEN_FEATURE_DOMAIN}
            flagValueMap={{ [HEALTH_MODELS_FLAG]: true }}
            delayMs={10}
          >
            {children}
          </OpenFeatureTestProvider>
        ),
      });
      expect(result.current).toBe(false);

      await waitFor(() => expect(result.current).toBe(true));
    });

    it('re-renders when the flag configuration changes on a ready provider', async () => {
      const provider = new InMemoryProvider(healthModelsFlagConfig(false));
      await OpenFeature.setProviderAndWait(OPEN_FEATURE_DOMAIN, provider);

      const { result } = renderHook(() => useHealthModelsFlag(), {
        wrapper: ({ children }) => <OpenFeatureProvider domain={OPEN_FEATURE_DOMAIN}>{children}</OpenFeatureProvider>,
      });
      expect(result.current).toBe(false);

      await act(async () => {
        await provider.putConfiguration(healthModelsFlagConfig(true));
      });

      await waitFor(() => expect(result.current).toBe(true));
    });

    it('falls back to the default value when the flag is missing from a ready provider', async () => {
      await OpenFeature.setProviderAndWait(OPEN_FEATURE_DOMAIN, new InMemoryProvider({}));

      const { result } = renderHook(() => useHealthModelsFlag(), {
        wrapper: ({ children }) => <OpenFeatureProvider domain={OPEN_FEATURE_DOMAIN}>{children}</OpenFeatureProvider>,
      });
      expect(result.current).toBe(false);
    });
  });
});
