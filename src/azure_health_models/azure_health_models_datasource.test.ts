import { type DataFrame, type DataQueryRequest, dateTime, FieldType, LoadingState } from '@grafana/data';
import { type BackendSrvRequest } from '@grafana/runtime';
import { firstValueFrom } from 'rxjs';

import { createMockInstanceSetttings } from '../mocks/instanceSettings';
import createMockQuery from '../mocks/query';
import { AzureHealthModelsQueryType, AzureQueryType } from '../dataquery';
import { type AzureMonitorQuery } from '../types/query';

import AzureHealthModelsDatasource from './azure_health_models_datasource';

let replace = (value?: string) => value || '';

jest.mock('@grafana/runtime', () => {
  return {
    __esModule: true,
    ...jest.requireActual('@grafana/runtime'),
    getTemplateSrv: () => ({
      replace: (value?: string) => replace(value),
      getVariables: jest.fn(),
      updateTimeRange: jest.fn(),
      containsTemplate: jest.fn(),
    }),
  };
});

describe('AzureHealthModelsDatasource', () => {
  const modelID = '/subscriptions/sub-a/resourceGroups/rg/providers/Microsoft.CloudHealth/healthmodels/model-a';
  const cachedQuery = () =>
    createMockQuery({
      queryType: AzureQueryType.AzureHealthModels,
      subscription: 'sub-a',
      azureHealthModels: {
        healthModelResourceId: modelID,
        queryType: AzureHealthModelsQueryType.EntityHealthSnapshot,
        dataplaneUrl: 'https://model-a.data.cloudhealth.azure.com',
      },
    });

  it('preserves a saved endpoint after a dashboard JSON round trip', () => {
    const datasource = new AzureHealthModelsDatasource(createMockInstanceSetttings());
    const original = cachedQuery();
    const restored = JSON.parse(JSON.stringify(original));
    expect(datasource.applyTemplateVariables(restored, {}).azureHealthModels).toEqual(original.azureHealthModels);
  });

  it.each(['subscription', 'template'])('invalidates saved discovery when the %s changes', (change) => {
    const datasource = new AzureHealthModelsDatasource(createMockInstanceSetttings());
    const query = cachedQuery();
    if (change === 'subscription') {
      query.subscription = 'sub-b';
    } else {
      query.subscription = '$sub';
      replace = (value) => (value === '$sub' ? 'sub-b' : value || '');
    }
    const result = datasource.applyTemplateVariables(query, {});
    expect(result.azureHealthModels?.dataplaneUrl).toBeUndefined();
  });

  it('resolves variable-backed endpoints afresh instead of persisting model metadata', () => {
    const datasource = new AzureHealthModelsDatasource(createMockInstanceSetttings());
    const query = cachedQuery();
    query.subscription = '$sub';
    query.azureHealthModels = { ...query.azureHealthModels, healthModelResourceId: '$model' };
    replace = (value) => (value === '$sub' ? 'sub-a' : value === '$model' ? modelID : value || '');
    expect(datasource.applyTemplateVariables(query, {}).azureHealthModels?.dataplaneUrl).toBeUndefined();
    replace = (value) => (value === '$sub' ? 'sub-a' : value === '$model' ? modelID + '-other' : value || '');
    expect(datasource.applyTemplateVariables(query, {}).azureHealthModels?.dataplaneUrl).toBeUndefined();
  });

  it('discovers the endpoint through the existing ARM resource route', async () => {
    const datasource = new AzureHealthModelsDatasource(createMockInstanceSetttings());
    const endpoint = 'https://model-a.data.cloudhealth.azure.com';
    datasource.getResource = jest.fn().mockResolvedValue({ properties: { dataPlaneUri: endpoint } });
    await expect(datasource.getDataplaneUrl(modelID)).resolves.toBe(endpoint);
    expect(datasource.getResource).toHaveBeenCalledWith(`azuremonitor${modelID}?api-version=2026-12-01-preview`);
  });

  it('reports missing endpoint metadata explicitly', async () => {
    const datasource = new AzureHealthModelsDatasource(createMockInstanceSetttings());
    datasource.getResource = jest.fn().mockResolvedValue({ properties: {} });
    await expect(datasource.getDataplaneUrl(modelID)).rejects.toThrow('properties.dataPlaneUri');
  });

  it('propagates ARM discovery failures', async () => {
    const datasource = new AzureHealthModelsDatasource(createMockInstanceSetttings());
    const error = new Error('ARM forbidden');
    datasource.getResource = jest.fn().mockRejectedValue(error);
    await expect(datasource.getDataplaneUrl(modelID)).rejects.toBe(error);
  });

  beforeEach(() => {
    replace = (value?: string) => value || '';
  });

  it('returns an empty list when the subscription cannot be resolved', async () => {
    replace = () => '';
    const datasource = new AzureHealthModelsDatasource(createMockInstanceSetttings());
    datasource.getResource = jest.fn();

    const healthModels = await datasource.getHealthModels('$subscription');

    expect(healthModels).toEqual([]);
    expect(datasource.getResource).not.toHaveBeenCalled();
  });

  it('queries CloudHealth healthmodels and maps results', async () => {
    const datasource = new AzureHealthModelsDatasource(createMockInstanceSetttings());
    datasource.getResource = jest.fn().mockResolvedValue({
      value: [
        { id: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-1', name: 'Health Model 1' },
        { id: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-2', name: 'Health Model 2' },
      ],
    });

    const healthModels = await datasource.getHealthModels('sub-a');

    expect(datasource.getResource).toHaveBeenCalledWith(
      'azuremonitor/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels?api-version=2026-11-01'
    );
    expect(healthModels).toEqual([
      {
        text: 'Health Model 1',
        value: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-1',
      },
      {
        text: 'Health Model 2',
        value: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-2',
      },
    ]);
  });

  it('uses the template-resolved subscription value in the ARM request path', async () => {
    replace = (value?: string) => (value === '$sub' ? 'resolved-subscription' : value || '');
    const datasource = new AzureHealthModelsDatasource(createMockInstanceSetttings());
    datasource.getResource = jest.fn().mockResolvedValue({ value: [] });

    await datasource.getHealthModels('$sub');

    expect(datasource.getResource).toHaveBeenCalledWith(
      'azuremonitor/subscriptions/resolved-subscription/providers/Microsoft.CloudHealth/healthmodels?api-version=2026-11-01'
    );
  });

  describe('frontend query execution', () => {
    const endpoint = 'https://model-a.data.cloudhealth.azure.com';
    const entity = (name: string) => ({ id: `${modelID}/entities/${name}`, name, properties: {} });
    const model = { id: modelID, name: 'model-a', location: 'westus', properties: { dataPlaneUri: endpoint } };
    const relationship = {
      id: `${modelID}/relationships/root-to-child`,
      name: 'root-to-child',
      properties: { parentEntityName: 'model-a', childEntityName: 'child-entity' },
    };
    const snapshot = (name: string) => ({ entityName: name, healthState: 'Healthy', signalInputs: [] });
    const transition = { previousState: 'Unknown', newState: 'Healthy', occurredAt: '2026-11-24T22:30:00Z' };
    const request = (targets = [cachedQuery()]): DataQueryRequest<AzureMonitorQuery> => {
      const from = dateTime('2026-11-24T22:00:00Z');
      const to = dateTime('2026-11-24T23:00:00Z');
      return {
        requestId: 'health-models',
        interval: '1m',
        intervalMs: 60000,
        range: { from, to, raw: { from, to } },
        scopedVars: {},
        targets,
        timezone: 'utc',
        app: 'explore',
        startTime: 0,
      };
    };
    const setup = (entities = [entity('child-entity')]) => {
      const datasource = new AzureHealthModelsDatasource(createMockInstanceSetttings());
      const getResource = jest
        .spyOn(datasource, 'getResource')
        .mockImplementation(
          async (path: string, params?: BackendSrvRequest['params'], options?: Partial<BackendSrvRequest>) => {
            const url = new URL(path, 'https://grafana.test');
            const armPath = url.pathname.slice('/azuremonitor'.length);
            if (armPath === modelID) {
              return model;
            }
            if (armPath === `${modelID}/entities`) {
              return { value: entities };
            }
            if (armPath === `${modelID}/relationships`) {
              return { value: [relationship] };
            }
            if (path === 'healthmodels') {
              const operation = new URL(params?.path, endpoint);
              const name = operation.pathname.split('/')[3];
              return operation.pathname.endsWith('/transitions') ? { value: [transition] } : snapshot(name);
            }
            throw new Error(`Unexpected resource request: ${path}`);
          }
        );
      return { datasource, getResource };
    };

    it.each([AzureHealthModelsQueryType.EntityHealthSnapshot, AzureHealthModelsQueryType.EntityHealthHistory])(
      'fetches %s through resource routes and keeps the four JSON result frames',
      async (queryType) => {
        const { datasource, getResource } = setup([entity('model-a'), entity('child-entity')]);
        const query = cachedQuery();
        query.azureHealthModels = { ...query.azureHealthModels, queryType };
        const result = await firstValueFrom(datasource.query(request([query])));
        expect(result.errors).toBeUndefined();
        const frames = result.data as DataFrame[];
        expect(frames.map((frame) => frame.name)).toEqual([
          'healthModel',
          'entities',
          'relationships',
          queryType === AzureHealthModelsQueryType.EntityHealthHistory ? 'transitions' : 'snapshots',
        ]);
        for (const frame of frames) {
          expect(frame.refId).toBe('A');
          expect(frame.fields).toHaveLength(1);
          expect(frame.fields[0].name).toBe('data');
          expect(frame.fields[0].type).toBe(FieldType.other);
        }
        expect(frames[0].fields[0].values).toEqual([model]);
        expect(frames[1].fields[0].values.map((value) => value.name)).toEqual(['model-a', 'child-entity']);
        expect(frames[2].fields[0].values).toEqual([relationship]);
        expect(frames[3].fields[0].values[1]).toEqual(
          queryType === AzureHealthModelsQueryType.EntityHealthHistory
            ? { entityName: 'child-entity', transitions: [transition] }
            : { entityName: 'child-entity', snapshot: snapshot('child-entity') }
        );
        const dataplaneCalls = getResource.mock.calls.filter(([path]) => path === 'healthmodels');
        expect(dataplaneCalls).toHaveLength(2);
        for (const [, params, options] of dataplaneCalls) {
          expect(params?.dataplaneUrl).toBe(endpoint);
          expect(options?.abortSignal).toBeInstanceOf(AbortSignal);
          expect(options?.showErrorAlert).toBe(false);
          if (queryType === AzureHealthModelsQueryType.EntityHealthSnapshot) {
            expect(params).toMatchObject({
              'api-version': '2026-10-01-preview',
              timestamp: '2026-11-24T23:00:00.000Z',
            });
          } else {
            const operation = new URL(params?.path, endpoint);
            expect(operation.searchParams.get('api-version')).toBe('2026-10-01-preview');
            expect(operation.searchParams.get('startAt')).toBe('2026-11-24T22:00:00.000Z');
            expect(operation.searchParams.get('endAt')).toBe('2026-11-24T23:00:00.000Z');
          }
        }
      }
    );

    it('fetches fresh metadata and health on refresh without requiring the editor', async () => {
      const { datasource, getResource } = setup();
      const query = cachedQuery();
      query.azureHealthModels = { ...query.azureHealthModels, dataplaneUrl: undefined };
      await firstValueFrom(datasource.query(request([query])));
      await firstValueFrom(datasource.query(request([query])));
      expect(getResource.mock.calls.filter(([path]) => path === 'healthmodels')).toHaveLength(2);
      expect(getResource.mock.calls.filter(([path]) => path.startsWith(`azuremonitor${modelID}?`))).toHaveLength(2);
    });

    it('resolves dashboard variables before executing', async () => {
      const { datasource, getResource } = setup();
      replace = (value) => (value === '$model' ? modelID : value === '$sub' ? 'sub-a' : value || '');
      const query = cachedQuery();
      query.subscription = '$sub';
      query.azureHealthModels = {
        ...query.azureHealthModels,
        healthModelResourceId: '$model',
        dataplaneUrl: 'https://old-model.data.cloudhealth.azure.com',
      };
      const result = await firstValueFrom(datasource.query(request([query])));
      expect(result.errors).toBeUndefined();
      expect(getResource.mock.calls.filter(([path]) => path === 'healthmodels')).toHaveLength(1);
    });

    it('retains per-query errors alongside another successful query', async () => {
      const { datasource } = setup();
      const bad = cachedQuery();
      bad.refId = 'B';
      bad.azureHealthModels = { ...bad.azureHealthModels, healthModelResourceId: `${modelID}/../other` };
      const result = await firstValueFrom(datasource.query(request([cachedQuery(), bad])));
      expect(result.data).toHaveLength(4);
      expect(result.state).toBe(LoadingState.Error);
      expect(result.errors).toEqual([
        expect.objectContaining({ refId: 'B', message: expect.stringContaining('full Health Model') }),
      ]);
    });

    it.each([AzureHealthModelsQueryType.EntityHealthSnapshot, AzureHealthModelsQueryType.EntityHealthHistory])(
      'uses the saved URL for %s without rediscovering or comparing endpoints',
      async (queryType) => {
        const { datasource, getResource } = setup();
        const savedEndpoint = 'https://saved-model.data.cloudhealth.azure.com';
        const query = cachedQuery();
        query.azureHealthModels = { ...query.azureHealthModels, queryType, dataplaneUrl: savedEndpoint };
        const discover = jest.spyOn(datasource, 'getDataplaneUrl');

        for (const properties of [{ dataPlaneUri: endpoint }, { dataPlaneUri: '' }, undefined]) {
          getResource.mockResolvedValueOnce({ ...model, properties });
          const result = await firstValueFrom(datasource.query(request([query])));
          expect(result.errors).toBeUndefined();
          expect(result.data).toHaveLength(4);
          const lastCall = getResource.mock.calls.at(-1);
          expect(lastCall?.[0]).toBe('healthmodels');
          expect(lastCall?.[1]?.dataplaneUrl).toBe(savedEndpoint);
        }

        expect(discover).not.toHaveBeenCalled();
        expect(getResource.mock.calls.filter(([path]) => path.startsWith(`azuremonitor${modelID}?`))).toHaveLength(3);
        expect(getResource.mock.calls.filter(([path]) => path === 'healthmodels')).toHaveLength(3);
      }
    );

    it.each(['missing endpoint', 'wrong model', 'ARM denied'])(
      'reports %s without requesting health data',
      async (failure) => {
        const { datasource, getResource } = setup();
        if (failure === 'ARM denied') {
          getResource.mockRejectedValueOnce({ status: 403, statusText: 'Forbidden', data: { message: 'ARM denied' } });
        } else if (failure === 'missing endpoint') {
          getResource.mockResolvedValueOnce({ ...model, properties: { dataPlaneUri: '' } });
        } else if (failure === 'wrong model') {
          getResource.mockResolvedValueOnce({ ...model, id: `${modelID}-other` });
        }
        const query = cachedQuery();
        if (failure === 'missing endpoint') {
          query.azureHealthModels = { ...query.azureHealthModels, dataplaneUrl: undefined };
        }
        const result = await firstValueFrom(datasource.query(request([query])));
        expect(result.state).toBe(LoadingState.Error);
        expect(result.errors?.[0].refId).toBe('A');
        expect(result.data).toEqual([]);
        expect(getResource.mock.calls.filter(([path]) => path === 'healthmodels')).toHaveLength(0);
      }
    );

    it.each(['ARM', 'data plane'])('returns nested %s errors as readable text', async (plane) => {
      const { datasource, getResource } = setup();
      const error = {
        status: 400,
        statusText: 'Bad Request',
        data: { error: { code: 'NoRegisteredProviderFound', message: 'The requested API version is unavailable' } },
      };
      if (plane === 'ARM') {
        getResource.mockRejectedValueOnce(error);
      } else {
        datasource.getEntityHealth = jest.fn().mockRejectedValue(error);
      }
      const result = await firstValueFrom(datasource.query(request()));
      expect(result.state).toBe(LoadingState.Error);
      expect(result.errors?.[0]).toMatchObject({
        refId: 'A',
        message: expect.stringContaining('The requested API version is unavailable'),
      });
      if (plane === 'ARM') {
        expect(result.errors?.[0].status).toBe(400);
        expect(result.data).toEqual([]);
      } else {
        expect((result.data[3] as DataFrame).fields[0].values[0].error).toBe('The requested API version is unavailable');
      }
    });

    it.each(['partial', 'all', 'empty'])('keeps explicit entity errors for %s failures', async (mode) => {
      const { datasource } = setup(mode === 'empty' ? [] : [entity('healthy-entity'), entity('failed-entity')]);
      datasource.getEntityHealth = jest.fn(async (_, name) => {
        if (mode === 'all' || name === 'failed-entity') {
          throw new Error('403 Forbidden');
        }
        return snapshot(name);
      });
      const result = await firstValueFrom(datasource.query(request()));
      const frame = result.data[3] as DataFrame;
      expect(frame.fields[0].values).toHaveLength(mode === 'empty' ? 0 : 2);
      if (mode !== 'empty') {
        expect(frame.fields[0].values[1]).toEqual({ entityName: 'failed-entity', error: '403 Forbidden' });
        expect(frame.meta?.notices?.[0].severity).toBe('warning');
      }
      if (mode === 'all') {
        expect(result.errors?.[0].message).toContain('All 2 Health Model entity reads failed');
      } else {
        expect(result.errors).toBeUndefined();
      }
    });

    it('rejects malformed snapshots and transitions rather than showing fake health', async () => {
      const { datasource } = setup();
      datasource.getEntityHealth = jest.fn().mockResolvedValue({ entityName: 'other-entity' });
      let result = await firstValueFrom(datasource.query(request()));
      expect(result.errors?.[0].message).toContain('entityName or healthState');
      datasource.getEntityHealthHistory = jest.fn().mockResolvedValue([{ newState: 'Healthy' }]);
      const query = cachedQuery();
      query.azureHealthModels = {
        ...query.azureHealthModels,
        queryType: AzureHealthModelsQueryType.EntityHealthHistory,
      };
      result = await firstValueFrom(datasource.query(request([query])));
      expect(result.errors?.[0].message).toContain('malformed transition');
    });

    it('follows transition pages and preserves carried-in history', async () => {
      const { datasource, getResource } = setup();
      const original = getResource.getMockImplementation()!;
      getResource.mockImplementation(async (path, params, options) => {
        if (path === 'healthmodels') {
          const operation = new URL(params?.path, endpoint);
          if (operation.searchParams.has('nextMarker')) {
            return { value: [{ ...transition, occurredAt: '2026-11-23T00:00:00Z' }] };
          }
          return {
            value: [transition],
            nextLink: '?api-version=2026-10-01-preview&nextMarker=second',
          };
        }
        return original(path, params, options);
      });
      const query = cachedQuery();
      query.azureHealthModels = {
        ...query.azureHealthModels,
        queryType: AzureHealthModelsQueryType.EntityHealthHistory,
      };
      const result = await firstValueFrom(datasource.query(request([query])));
      expect(result.errors).toBeUndefined();
      expect((result.data[3] as DataFrame).fields[0].values[0].transitions).toHaveLength(2);
      expect((result.data[3] as DataFrame).fields[0].values[0].transitions[1].occurredAt).toBe('2026-11-23T00:00:00Z');
      const lastCall = getResource.mock.calls.at(-1);
      expect(lastCall?.[1]?.path).not.toContain('startAt');
      expect(lastCall?.[1]?.path).toContain('nextMarker=second');
    });

    it.each(['missing value', 'cycle', 'different host', 'different operation'])(
      'rejects %s transition pagination',
      async (failure) => {
        const { datasource, getResource } = setup();
        const original = getResource.getMockImplementation()!;
        getResource.mockImplementation(async (path, params, options) => {
          if (path === 'healthmodels') {
            if (failure === 'missing value') {
              return {};
            }
            return {
              value: [],
              nextLink:
                failure === 'cycle'
                  ? params?.path
                  : failure === 'different host'
                    ? 'https://attacker.test/api/entities/child-entity/transitions'
                    : '/api/entities/other-entity/transitions',
            };
          }
          return original(path, params, options);
        });
        const query = cachedQuery();
        query.azureHealthModels = {
          ...query.azureHealthModels,
          queryType: AzureHealthModelsQueryType.EntityHealthHistory,
        };
        const result = await firstValueFrom(datasource.query(request([query])));
        expect(result.errors?.[0].message).toMatch(/value array|repeated a page|original endpoint/);
        expect(getResource.mock.calls.filter(([path]) => path === 'healthmodels')).toHaveLength(1);
      }
    );

    it('follows ARM entity and relationship pages through the existing proxy', async () => {
      const { datasource, getResource } = setup();
      const original = getResource.getMockImplementation()!;
      getResource.mockImplementation(async (path, params, options) => {
        const url = new URL(path, 'https://grafana.test');
        if (url.pathname.endsWith('/entities') || url.pathname.endsWith('/relationships')) {
          if (url.searchParams.has('page')) {
            return { value: url.pathname.endsWith('/entities') ? [entity('child-entity')] : [relationship] };
          }
          return {
            value: url.pathname.endsWith('/entities') ? [entity('model-a')] : [],
            nextLink: `${url.pathname.slice('/azuremonitor'.length)}?api-version=2026-12-01-preview&page=2`,
          };
        }
        return original(path, params, options);
      });
      const result = await firstValueFrom(datasource.query(request()));
      expect(result.errors).toBeUndefined();
      expect((result.data[1] as DataFrame).fields[0].values.map((value) => value.name)).toEqual([
        'model-a',
        'child-entity',
      ]);
      expect((result.data[2] as DataFrame).fields[0].values).toEqual([relationship]);
    });

    it('reports the page limit instead of silently returning incomplete history', async () => {
      const { datasource, getResource } = setup();
      const original = getResource.getMockImplementation()!;
      let pages = 0;
      getResource.mockImplementation(async (path, params, options) => {
        if (path === 'healthmodels') {
          pages++;
          return { value: [transition], nextLink: `?api-version=2026-10-01-preview&nextMarker=${pages}` };
        }
        return original(path, params, options);
      });
      const query = cachedQuery();
      query.azureHealthModels = {
        ...query.azureHealthModels,
        queryType: AzureHealthModelsQueryType.EntityHealthHistory,
      };
      const result = await firstValueFrom(datasource.query(request([query])));
      expect(pages).toBe(50);
      expect(result.errors?.[0].message).toContain('exceeded 50 pages');
    });

    it('does not request entity health for an invalid history time range', async () => {
      const { datasource, getResource } = setup();
      const query = cachedQuery();
      query.azureHealthModels = {
        ...query.azureHealthModels,
        queryType: AzureHealthModelsQueryType.EntityHealthHistory,
      };
      const options = request([query]);
      options.range.from = options.range.to;
      const result = await firstValueFrom(datasource.query(options));
      expect(result.errors?.[0].message).toContain('from before to');
      expect(getResource).not.toHaveBeenCalled();
    });

    it('limits concurrent entity requests to ten and keeps their original order', async () => {
      const entities = Array.from({ length: 25 }, (_, index) => entity(`entity-${index.toString().padStart(2, '0')}`));
      const { datasource } = setup(entities);
      let active = 0;
      let maximum = 0;
      datasource.getEntityHealth = jest.fn(async (_, name) => {
        active++;
        maximum = Math.max(maximum, active);
        await Promise.resolve();
        active--;
        return snapshot(name);
      });
      const result = await firstValueFrom(datasource.query(request()));
      expect(maximum).toBe(10);
      expect((result.data[3] as DataFrame).fields[0].values.map((value) => value.entityName)).toEqual(
        entities.map((value) => value.name)
      );
    });

    it('aborts in-flight resource calls when the query is unsubscribed', async () => {
      const { datasource, getResource } = setup();
      let signal: AbortSignal | undefined;
      getResource.mockImplementationOnce(
        (_, __, options) =>
          new Promise((_, reject) => {
            signal = options?.abortSignal;
            signal?.addEventListener('abort', () => reject(new DOMException('Canceled', 'AbortError')));
          })
      );
      const subscription = datasource.query(request()).subscribe();
      expect(signal?.aborted).toBe(false);
      subscription.unsubscribe();
      expect(signal?.aborted).toBe(true);
      await Promise.resolve();
      expect(getResource).toHaveBeenCalledTimes(1);
    });
  });
});
