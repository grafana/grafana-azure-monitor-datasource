import { createMockInstanceSetttings } from '../mocks/instanceSettings';
import createMockQuery from '../mocks/query';
import { AzureHealthModelsQueryType, AzureQueryType } from '../dataquery';

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
        dataplaneModel: { id: modelID, name: 'model-a', location: 'uksouth' },
      },
    });

  it('preserves a saved model-bound endpoint after a dashboard JSON round trip', () => {
    const datasource = new AzureHealthModelsDatasource(createMockInstanceSetttings());
    const original = cachedQuery();
    const restored = JSON.parse(JSON.stringify(original));
    expect(datasource.applyTemplateVariables(restored, {}).azureHealthModels).toEqual(original.azureHealthModels);
  });

  it.each(['model', 'subscription', 'template'])('invalidates cached discovery when the %s changes', (change) => {
    const datasource = new AzureHealthModelsDatasource(createMockInstanceSetttings());
    const query = cachedQuery();
    if (change === 'model') {
      query.azureHealthModels = { ...query.azureHealthModels, healthModelResourceId: modelID + '-other' };
    } else if (change === 'subscription') {
      query.subscription = 'sub-b';
    } else {
      query.subscription = '$sub';
      replace = (value) => (value === '$sub' ? 'sub-b' : value || '');
    }
    const result = datasource.applyTemplateVariables(query, {});
    expect(result.azureHealthModels?.dataplaneUrl).toBeUndefined();
    expect(result.azureHealthModels?.dataplaneModel).toBeUndefined();
  });

  it('reuses discovery while template values are unchanged and invalidates it when they resolve to another model', () => {
    const datasource = new AzureHealthModelsDatasource(createMockInstanceSetttings());
    const query = cachedQuery();
    query.subscription = '$sub';
    query.azureHealthModels = { ...query.azureHealthModels, healthModelResourceId: '$model' };
    replace = (value) => (value === '$sub' ? 'sub-a' : value === '$model' ? modelID : value || '');
    expect(datasource.applyTemplateVariables(query, {}).azureHealthModels?.dataplaneUrl).toBe(
      query.azureHealthModels.dataplaneUrl
    );
    replace = (value) => (value === '$sub' ? 'sub-a' : value === '$model' ? modelID + '-other' : value || '');
    expect(datasource.applyTemplateVariables(query, {}).azureHealthModels?.dataplaneUrl).toBeUndefined();
  });

  it('does not use a legacy endpoint without matching cached model metadata', () => {
    const datasource = new AzureHealthModelsDatasource(createMockInstanceSetttings());
    const query = cachedQuery();
    delete query.azureHealthModels?.dataplaneModel;
    expect(datasource.applyTemplateVariables(query, {}).azureHealthModels?.dataplaneUrl).toBeUndefined();
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
});
