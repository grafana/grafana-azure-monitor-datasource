import { dateTime, LoadingState } from '@grafana/data';
import { firstValueFrom, of } from 'rxjs';

import { AzureHealthModelsQueryType, AzureQueryType } from './dataquery';
import Datasource from './datasource';
import { createMockInstanceSetttings } from './mocks/instanceSettings';
import createMockQuery from './mocks/query';
import { singleVariable } from './mocks/variables';

jest.mock('@grafana/runtime', () => {
  return {
    __esModule: true,
    ...jest.requireActual('@grafana/runtime'),
    getTemplateSrv: () => ({
      replace: (target?: string) => {
        if (target === '$resourceGroup') {
          return 'the-resource-group';
        }
        return target || '';
      },
      getVariables: jest.fn(),
      updateTimeRange: jest.fn(),
      containsTemplate: (target?: string) => {
        return (target || '').includes('$');
      },
    }),
  };
});

describe('Azure Monitor Datasource', () => {
  describe('Health Models query routing', () => {
    it.each([AzureHealthModelsQueryType.EntityHealthSnapshot, AzureHealthModelsQueryType.EntityHealthHistory])(
      'forwards %s to the Health Models datasource',
      async (queryType) => {
        const ds = new Datasource(createMockInstanceSetttings());
        const query = createMockQuery({
          queryType: AzureQueryType.AzureHealthModels,
          azureHealthModels: {
            healthModelResourceId:
              '/subscriptions/test-sub/resourceGroups/test-rg/providers/Microsoft.CloudHealth/healthmodels/test-model',
            queryType,
          },
        });
        const healthModelsQuery = jest.spyOn(ds.azureHealthModelsDatasource, 'query').mockReturnValue(of({ data: [] }));
        const from = dateTime('2026-01-01T00:00:00Z');
        const to = dateTime('2026-01-01T01:00:00Z');
        await firstValueFrom(
          ds.query({
            requestId: 'health-models',
            interval: '1m',
            intervalMs: 60000,
            range: { from, to, raw: { from, to } },
            scopedVars: {},
            targets: [query],
            timezone: 'utc',
            app: 'explore',
            startTime: 0,
          })
        );
        expect(healthModelsQuery).toHaveBeenCalledWith(
          expect.objectContaining({ requestId: 'health-models-A', targets: [query] })
        );
      }
    );

    it('keeps Health Models errors when merging with another Azure query type', async () => {
      const ds = new Datasource(createMockInstanceSetttings());
      const error = { refId: 'A', message: 'ARM denied' };
      jest.spyOn(ds.azureHealthModelsDatasource, 'query').mockReturnValue(of({ data: [], errors: [error] }));
      jest.spyOn(ds.azureMonitorDatasource, 'query').mockReturnValue(of({ data: [] }));
      const from = dateTime('2026-01-01T00:00:00Z');
      const to = dateTime('2026-01-01T01:00:00Z');
      const result = await firstValueFrom(
        ds.query({
          requestId: 'mixed',
          interval: '1m',
          intervalMs: 60000,
          range: { from, to, raw: { from, to } },
          scopedVars: {},
          targets: [
            createMockQuery({
              queryType: AzureQueryType.AzureHealthModels,
              azureHealthModels: { healthModelResourceId: 'model' },
            }),
            createMockQuery({ refId: 'B' }),
          ],
          timezone: 'utc',
          app: 'explore',
          startTime: 0,
        })
      );
      expect(result.errors).toEqual([error]);
      expect(result.state).toBe(LoadingState.Error);
    });
  });

  describe('interpolateVariablesInQueries()', () => {
    it('should interpolate variables in the queries', () => {
      const ds = new Datasource(createMockInstanceSetttings());
      const queries = [createMockQuery({ azureMonitor: { resources: [{ resourceGroup: '$resourceGroup' }] } })];

      const interpolatedQueries = ds.interpolateVariablesInQueries(queries, {
        resourceGroup: { text: 'the-resource-group', value: 'the-resource-group' },
      });

      expect(interpolatedQueries).toContainEqual(
        expect.objectContaining({
          azureMonitor: expect.objectContaining({
            resources: [expect.objectContaining({ resourceGroup: 'the-resource-group' })],
          }),
        })
      );
    });

    it('should include a datasource ref when interpolating queries', () => {
      const ds = new Datasource(createMockInstanceSetttings());
      const query = createMockQuery();
      delete query.datasource;
      const queries = [query];

      const interpolatedQueries = ds.interpolateVariablesInQueries(queries, {});

      expect(interpolatedQueries).toContainEqual(
        expect.objectContaining({
          datasource: expect.objectContaining({ type: 'azuremonitor', uid: 'abc' }),
        })
      );
    });
  });

  it('should not filter a valid query', () => {
    const ds = new Datasource(createMockInstanceSetttings());
    const query = createMockQuery();
    expect(ds.filterQuery(query)).toBe(true);
  });

  it('should filter out a query with no query type', () => {
    const ds = new Datasource(createMockInstanceSetttings());
    const query = createMockQuery();
    delete query.queryType;
    expect(ds.filterQuery(query)).toBe(false);
  });

  describe('When performing targetContainsTemplate', () => {
    it('should return false when no variable is being used', () => {
      const query = {
        ...createMockQuery(),
        queryType: AzureQueryType.AzureResourceGraph,
      };
      const ds = new Datasource(createMockInstanceSetttings());
      expect(ds.targetContainsTemplate(query)).toEqual(false);
    });

    it('should return true when resource field is using a variable', () => {
      const query = {
        ...createMockQuery(),
        queryType: AzureQueryType.AzureResourceGraph,
        azureResourceGraph: { query: '$temp-var' },
      };
      const ds = new Datasource(createMockInstanceSetttings());
      expect(ds.targetContainsTemplate(query)).toEqual(true);
    });

    it('should return true when resource field is using a variable in the subscriptions field', () => {
      const query = {
        ...createMockQuery(),
        queryType: AzureQueryType.AzureResourceGraph,
        subscriptions: ['$temp-var'],
      };
      const ds = new Datasource(createMockInstanceSetttings());
      expect(ds.targetContainsTemplate(query)).toEqual(true);
    });

    it('should return false when a variable is used in a different part of the query', () => {
      const query = {
        ...createMockQuery(),
        queryType: AzureQueryType.AzureResourceGraph,
        azureMonitor: { metricName: `$${singleVariable.name}` },
      };
      const ds = new Datasource(createMockInstanceSetttings());
      expect(ds.targetContainsTemplate(query)).toEqual(false);
    });
  });
});
