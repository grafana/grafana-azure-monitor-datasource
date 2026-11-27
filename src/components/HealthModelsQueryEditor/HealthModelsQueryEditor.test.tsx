import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { firstValueFrom, Subject } from 'rxjs';

import { mockComboboxRect } from '@grafana/test-utils';

import { AzureHealthModelsQueryType, AzureQueryType } from '../../dataquery';
import createMockDatasource from '../../mocks/datasource';
import createMockQuery from '../../mocks/query';
import { selectOptionInTest } from '../../utils/testUtils';

import HealthModelsQueryEditor from './HealthModelsQueryEditor';

let templateReplace = (value: string) => value;

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => ({ replace: (value: string) => templateReplace(value) }),
}));

describe('HealthModelsQueryEditor', () => {
  beforeEach(() => {
    templateReplace = (value) => value;
  });

  const modelID = '/subscriptions/sub-a/resourceGroups/rg/providers/Microsoft.CloudHealth/healthmodels/model-a';
  const endpoint = 'https://model-a.data.cloudhealth.azure.com';
  it.each(['literal', 'template'])('saves %s discovery without another query or repeated updates', async (mode) => {
    if (mode === 'template') {
      templateReplace = (value) => (value === '$sub' ? 'sub-a' : value === '$model' ? modelID : value);
    }
    const datasource = createMockDatasource({
      getSubscriptions: jest.fn().mockResolvedValue([{ text: 'Subscription A', value: 'sub-a' }]),
      getHealthModels: jest.fn().mockResolvedValue([{ text: 'Model A', value: modelID }]),
      azureHealthModelsDatasource: { getDataplaneUrl: jest.fn().mockResolvedValue(endpoint) },
    });
    const query = createMockQuery({
      queryType: AzureQueryType.AzureHealthModels,
      subscription: mode === 'template' ? '$sub' : 'sub-a',
      azureHealthModels: {
        healthModelResourceId: mode === 'template' ? '$model' : modelID,
        queryType: AzureHealthModelsQueryType.EntityHealthSnapshot,
      },
    });
    const onChange = jest.fn();
    const onQueryChange = jest.fn();
    const props = { datasource, onChange, onQueryChange, setError: jest.fn() };
    const { rerender } = render(<HealthModelsQueryEditor {...props} query={query} />);
    await waitFor(() => expect(onQueryChange).toHaveBeenCalledTimes(1));
    const saved = onQueryChange.mock.calls[0][0];
    expect(saved.azureHealthModels).toEqual({
      ...query.azureHealthModels,
      dataplaneUrl: endpoint,
    });
    rerender(<HealthModelsQueryEditor {...props} query={saved} />);
    expect(onQueryChange).toHaveBeenCalledTimes(1);
    expect(datasource.azureHealthModelsDatasource.getDataplaneUrl).toHaveBeenCalledWith(modelID);
    expect(datasource.azureHealthModelsDatasource.getDataplaneUrl).toHaveBeenCalledTimes(1);
    expect(onChange).not.toHaveBeenCalled();
  });

  it.each(['saved', 'other-subscription', 'missing-model'])(
    'does not request endpoint discovery for %s queries',
    async (source) => {
      const datasource = createMockDatasource({
        getSubscriptions: jest.fn().mockResolvedValue([{ text: 'Subscription A', value: 'sub-a' }]),
        getHealthModels: jest.fn().mockResolvedValue([{ text: 'Model A', value: modelID }]),
      });
      const query = createMockQuery({
        queryType: AzureQueryType.AzureHealthModels,
        subscription: source === 'other-subscription' ? 'sub-b' : 'sub-a',
        azureHealthModels: {
          healthModelResourceId: source === 'missing-model' ? undefined : modelID,
          queryType: AzureHealthModelsQueryType.EntityHealthSnapshot,
          dataplaneUrl: source === 'saved' ? endpoint : undefined,
        },
      });
      const onQueryChange = jest.fn();
      render(
        <HealthModelsQueryEditor
          query={query}
          datasource={datasource}
          onChange={jest.fn()}
          onQueryChange={onQueryChange}
          setError={jest.fn()}
        />
      );
      await waitFor(() => expect(datasource.getHealthModels).toHaveBeenCalled());
      expect(onQueryChange).not.toHaveBeenCalled();
      expect(datasource.azureHealthModelsDatasource.getDataplaneUrl).not.toHaveBeenCalled();
    }
  );

  it.each(['model', 'subscription', 'datasource', 'variable'])(
    'ignores late discovery after changing the %s and accepts the current response',
    async (selection) => {
      const oldResponse = new Subject<string>();
      const currentResponse = new Subject<string>();
      const getDataplaneUrl = jest
        .fn()
        .mockReturnValueOnce(firstValueFrom(oldResponse))
        .mockReturnValue(firstValueFrom(currentResponse));
      const datasource = createMockDatasource({
        uid: 'source-a',
        getSubscriptions: jest.fn().mockResolvedValue([{ text: 'Subscription A', value: 'sub-a' }]),
        getHealthModels: jest.fn().mockResolvedValue([{ text: 'Model A', value: modelID }]),
        azureHealthModelsDatasource: { getDataplaneUrl },
      });
      let resolvedModel = modelID;
      templateReplace = (value) => (value === '$model' ? resolvedModel : value);
      const query = createMockQuery({
        datasource: { uid: datasource.uid },
        queryType: AzureQueryType.AzureHealthModels,
        subscription: 'sub-a',
        azureHealthModels: {
          healthModelResourceId: selection === 'variable' ? '$model' : modelID,
          queryType: AzureHealthModelsQueryType.EntityHealthSnapshot,
        },
      });
      const onQueryChange = jest.fn();
      const onChange = jest.fn();
      const props = { datasource, onChange, onQueryChange, setError: jest.fn() };
      const { rerender } = render(<HealthModelsQueryEditor {...props} query={query} />);
      await waitFor(() => expect(getDataplaneUrl).toHaveBeenCalledWith(modelID));

      const nextModelID =
        selection === 'subscription'
          ? modelID.replace('/sub-a/', '/sub-b/')
          : selection === 'datasource'
            ? modelID
            : modelID + '-other';
      resolvedModel = nextModelID;
      const nextDatasource =
        selection === 'datasource'
          ? createMockDatasource({
              uid: 'source-b',
              getSubscriptions: jest.fn().mockResolvedValue([{ text: 'Subscription A', value: 'sub-a' }]),
              getHealthModels: jest.fn().mockResolvedValue([{ text: 'Model A', value: modelID }]),
              azureHealthModelsDatasource: {
                getDataplaneUrl: jest.fn().mockReturnValue(firstValueFrom(currentResponse)),
              },
            })
          : datasource;
      const nextQuery = {
        ...query,
        datasource: { uid: nextDatasource.uid },
        subscription: selection === 'subscription' ? 'sub-b' : 'sub-a',
        azureHealthModels: {
          ...query.azureHealthModels,
          healthModelResourceId: selection === 'variable' ? '$model' : nextModelID,
        },
      };
      rerender(<HealthModelsQueryEditor {...props} datasource={nextDatasource} query={nextQuery} />);
      await waitFor(() =>
        expect(nextDatasource.azureHealthModelsDatasource.getDataplaneUrl).toHaveBeenCalledWith(nextModelID)
      );
      await act(async () => {
        oldResponse.next(endpoint);
      });
      expect(onQueryChange).not.toHaveBeenCalled();

      const nextEndpoint = 'https://current-model.data.cloudhealth.azure.com';
      await act(async () => {
        currentResponse.next(nextEndpoint);
      });
      await waitFor(() => expect(onQueryChange).toHaveBeenCalledTimes(1));
      expect(onQueryChange).toHaveBeenCalledWith({
        ...nextQuery,
        azureHealthModels: {
          ...nextQuery.azureHealthModels,
          dataplaneUrl: nextEndpoint,
        },
      });
      expect(onChange).not.toHaveBeenCalled();
    }
  );

  it('reports endpoint discovery errors without changing the query', async () => {
    const error = new Error('ARM endpoint discovery failed');
    const datasource = createMockDatasource({
      getSubscriptions: jest.fn().mockResolvedValue([{ text: 'Subscription A', value: 'sub-a' }]),
      getHealthModels: jest.fn().mockResolvedValue([{ text: 'Model A', value: modelID }]),
      azureHealthModelsDatasource: { getDataplaneUrl: jest.fn().mockRejectedValue(error) },
    });
    const query = createMockQuery({
      queryType: AzureQueryType.AzureHealthModels,
      subscription: 'sub-a',
      azureHealthModels: {
        healthModelResourceId: modelID,
        queryType: AzureHealthModelsQueryType.EntityHealthSnapshot,
      },
    });
    const onChange = jest.fn();
    const onQueryChange = jest.fn();
    const setError = jest.fn();
    render(
      <HealthModelsQueryEditor
        query={query}
        datasource={datasource}
        onChange={onChange}
        onQueryChange={onQueryChange}
        setError={setError}
      />
    );
    await waitFor(() => expect(setError).toHaveBeenCalledWith('health-models-endpoint', error));
    expect(onChange).not.toHaveBeenCalled();
    expect(onQueryChange).not.toHaveBeenCalled();
  });

  it.each(['model', 'subscription'])('clears the saved endpoint when selecting another %s', async (selection) => {
    const otherModelID = modelID + '-other';
    const datasource = createMockDatasource({
      getSubscriptions: jest.fn().mockResolvedValue([
        { text: 'Subscription A', value: 'sub-a' },
        { text: 'Subscription B', value: 'sub-b' },
      ]),
      getHealthModels: jest.fn().mockResolvedValue([
        { text: 'Model A', value: modelID },
        { text: 'Model B', value: otherModelID },
      ]),
    });
    const query = createMockQuery({
      queryType: AzureQueryType.AzureHealthModels,
      subscription: 'sub-a',
      azureHealthModels: {
        healthModelResourceId: modelID,
        queryType: AzureHealthModelsQueryType.EntityHealthSnapshot,
        dataplaneUrl: endpoint,
      },
    });
    const onChange = jest.fn();
    render(<HealthModelsQueryEditor query={query} datasource={datasource} onChange={onChange} setError={jest.fn()} />);
    await waitFor(() => expect(datasource.getHealthModels).toHaveBeenCalled());
    const label = selection === 'model' ? 'Azure Monitor Health Model' : 'Azure Monitor Health Models subscription';
    await selectOptionInTest(
      screen.getByRole('combobox', { name: label }),
      selection === 'model' ? 'Model B' : 'Subscription B'
    );
    await waitFor(() => expect(onChange).toHaveBeenCalled());
    const updated = onChange.mock.calls.at(-1)?.[0];
    expect(updated.azureHealthModels.dataplaneUrl).toBeUndefined();
    if (selection === 'model') {
      expect(updated.azureHealthModels.healthModelResourceId).toBe(otherModelID);
    } else {
      expect(updated.subscription).toBe('sub-b');
      expect(updated.azureHealthModels.healthModelResourceId).toBeUndefined();
    }
  });

  beforeAll(() => {
    mockComboboxRect();
  });

  it('shows loading health models for an automatically selected subscription', async () => {
    const user = userEvent.setup();
    const healthModelsResponse = new Subject<Array<{ text: string; value: string }>>();
    const getSubscriptions = jest.fn().mockResolvedValue([{ text: 'Subscription A', value: 'sub-a' }]);
    const getHealthModels = jest.fn().mockReturnValue(firstValueFrom(healthModelsResponse));
    const datasource = createMockDatasource({ getSubscriptions, getHealthModels });
    const query = createMockQuery({
      queryType: AzureQueryType.AzureHealthModels,
      subscription: undefined,
      azureHealthModels: {},
    });

    render(<HealthModelsQueryEditor query={query} datasource={datasource} onChange={jest.fn()} setError={jest.fn()} />);

    await waitFor(() => expect(getHealthModels).toHaveBeenCalledWith('sub-a'));
    await user.click(screen.getByRole('combobox', { name: 'Azure Monitor Health Model' }));

    expect(await screen.findByText('Loading options...')).toBeInTheDocument();
    expect(screen.queryByText('No health models found')).not.toBeInTheDocument();

    await act(async () => {
      healthModelsResponse.next([
        { text: 'Health Model A', value: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-a' },
      ]);
    });

    expect(await screen.findByRole('option', { name: /Health Model A/ })).toBeInTheDocument();
    expect(screen.queryByText('Loading options...')).not.toBeInTheDocument();
  });

  it('clears stale health models while loading an externally selected subscription', async () => {
    const user = userEvent.setup();
    const healthModelsResponse = new Subject<Array<{ text: string; value: string }>>();
    const getSubscriptions = jest.fn().mockResolvedValue([
      { text: 'Subscription A', value: 'sub-a' },
      { text: 'Subscription B', value: 'sub-b' },
    ]);
    const getHealthModels = jest
      .fn()
      .mockResolvedValueOnce([
        { text: 'Health Model A', value: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-a' },
      ])
      .mockReturnValueOnce(firstValueFrom(healthModelsResponse));
    const datasource = createMockDatasource({ getSubscriptions, getHealthModels });
    const onChange = jest.fn();
    const setError = jest.fn();
    const query = createMockQuery({
      queryType: AzureQueryType.AzureHealthModels,
      subscription: undefined,
      azureHealthModels: {},
    });
    const { rerender } = render(
      <HealthModelsQueryEditor
        query={query}
        datasource={datasource}
        subscriptionId="sub-a"
        onChange={onChange}
        setError={setError}
      />
    );

    await user.click(screen.getByRole('combobox', { name: 'Azure Monitor Health Model' }));
    expect(await screen.findByRole('option', { name: /Health Model A/ })).toBeInTheDocument();
    await waitFor(() =>
      expect(onChange).toHaveBeenCalledWith(
        expect.objectContaining({
          subscription: 'sub-a',
          azureHealthModels: expect.objectContaining({
            healthModelResourceId: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-a',
          }),
        })
      )
    );
    await user.keyboard('{Escape}');
    onChange.mockClear();

    rerender(
      <HealthModelsQueryEditor
        query={query}
        datasource={datasource}
        subscriptionId="sub-b"
        onChange={onChange}
        setError={setError}
      />
    );

    await waitFor(() => expect(getHealthModels).toHaveBeenCalledWith('sub-b'));
    await user.click(screen.getByRole('combobox', { name: 'Azure Monitor Health Model' }));

    expect(await screen.findByText('Loading options...')).toBeInTheDocument();
    expect(screen.queryByRole('option', { name: /Health Model A/ })).not.toBeInTheDocument();
    expect(onChange).not.toHaveBeenCalled();

    await act(async () => {
      healthModelsResponse.next([
        { text: 'Health Model B', value: '/subscriptions/sub-b/providers/Microsoft.CloudHealth/healthmodels/hm-b' },
      ]);
    });

    expect(await screen.findByRole('option', { name: /Health Model B/ })).toBeInTheDocument();
    await waitFor(() =>
      expect(onChange).toHaveBeenCalledWith(
        expect.objectContaining({
          subscription: 'sub-b',
          azureHealthModels: expect.objectContaining({
            healthModelResourceId: '/subscriptions/sub-b/providers/Microsoft.CloudHealth/healthmodels/hm-b',
          }),
        })
      )
    );
  });

  it('auto-selects the first health model after loading when the query has no selected model', async () => {
    const getSubscriptions = jest.fn().mockResolvedValue([{ text: 'Subscription A', value: 'sub-a' }]);
    const getHealthModels = jest.fn().mockResolvedValue([
      { text: 'Health Model 1', value: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-1' },
      { text: 'Health Model 2', value: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-2' },
    ]);
    const datasource = createMockDatasource({ getSubscriptions, getHealthModels });
    const onChange = jest.fn();
    const setError = jest.fn();
    const query = createMockQuery({
      queryType: AzureQueryType.AzureHealthModels,
      subscription: 'sub-a',
    });
    query.azureHealthModels = {};

    render(<HealthModelsQueryEditor query={query} datasource={datasource} onChange={onChange} setError={setError} />);

    await waitFor(() => expect(getSubscriptions).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(getHealthModels).toHaveBeenCalledWith('sub-a'));
    await waitFor(() =>
      expect(onChange).toHaveBeenCalledWith(
        expect.objectContaining({
          subscription: 'sub-a',
          azureHealthModels: expect.objectContaining({
            healthModelResourceId: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-1',
            queryType: AzureHealthModelsQueryType.EntityHealthSnapshot,
          }),
        })
      )
    );
  });

  it('defaults query type to snapshot when a health model is already selected and no type exists', async () => {
    const getSubscriptions = jest.fn().mockResolvedValue([{ text: 'Subscription A', value: 'sub-a' }]);
    const getHealthModels = jest.fn().mockResolvedValue([
      { text: 'Health Model 1', value: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-1' },
      { text: 'Health Model 2', value: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-2' },
    ]);
    const datasource = createMockDatasource({ getSubscriptions, getHealthModels });
    const onChange = jest.fn();
    const setError = jest.fn();
    const query = createMockQuery({
      queryType: AzureQueryType.AzureHealthModels,
      subscription: 'sub-a',
    });
    query.azureHealthModels = {
      healthModelResourceId: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-2',
    };

    render(<HealthModelsQueryEditor query={query} datasource={datasource} onChange={onChange} setError={setError} />);

    await waitFor(() => expect(getHealthModels).toHaveBeenCalledWith('sub-a'));
    await waitFor(() =>
      expect(onChange).toHaveBeenCalledWith(
        expect.objectContaining({
          queryType: AzureQueryType.AzureHealthModels,
          azureHealthModels: expect.objectContaining({
            healthModelResourceId: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-2',
            queryType: AzureHealthModelsQueryType.EntityHealthSnapshot,
          }),
        })
      )
    );
  });

  it.each(['hm-2', 'missing-model'])('does not overwrite the selected health model (%s)', async (modelName) => {
    const user = userEvent.setup();
    const getSubscriptions = jest.fn().mockResolvedValue([{ text: 'Subscription A', value: 'sub-a' }]);
    const getHealthModels = jest.fn().mockResolvedValue([
      { text: 'Health Model 1', value: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-1' },
      { text: 'Health Model 2', value: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-2' },
    ]);
    const datasource = createMockDatasource({ getSubscriptions, getHealthModels });
    const onChange = jest.fn();
    const setError = jest.fn();
    const query = createMockQuery({
      queryType: AzureQueryType.AzureHealthModels,
      subscription: 'sub-a',
    });
    query.azureHealthModels = {
      healthModelResourceId: `/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/${modelName}`,
      queryType: AzureHealthModelsQueryType.EntityHealthHistory,
    };

    render(<HealthModelsQueryEditor query={query} datasource={datasource} onChange={onChange} setError={setError} />);

    await waitFor(() => expect(getHealthModels).toHaveBeenCalledWith('sub-a'));
    await user.click(screen.getByRole('combobox', { name: 'Azure Monitor Health Model' }));
    expect(await screen.findByRole('option', { name: /Health Model 1/ })).toBeInTheDocument();
    expect(onChange).not.toHaveBeenCalled();
  });

  it('surfaces health model fetch errors through setError', async () => {
    const getSubscriptions = jest.fn().mockResolvedValue([{ text: 'Subscription A', value: 'sub-a' }]);
    const healthModelError = new Error('Failed to fetch health models');
    const getHealthModels = jest.fn().mockRejectedValue(healthModelError);
    const datasource = createMockDatasource({ getSubscriptions, getHealthModels });
    const setError = jest.fn();
    const query = createMockQuery({
      queryType: AzureQueryType.AzureHealthModels,
      subscription: 'sub-a',
    });
    query.azureHealthModels = {};

    render(<HealthModelsQueryEditor query={query} datasource={datasource} onChange={jest.fn()} setError={setError} />);

    await waitFor(() => expect(getHealthModels).toHaveBeenCalledWith('sub-a'));
    await waitFor(() => expect(setError).toHaveBeenCalledWith('health-models-list', healthModelError));
  });

  it('updates query type when selecting a health model type', async () => {
    const getSubscriptions = jest.fn().mockResolvedValue([{ text: 'Subscription A', value: 'sub-a' }]);
    const getHealthModels = jest.fn().mockResolvedValue([
      { text: 'Health Model 1', value: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-1' },
      { text: 'Health Model 2', value: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-2' },
    ]);
    const datasource = createMockDatasource({ getSubscriptions, getHealthModels });
    const onChange = jest.fn();
    const query = createMockQuery({
      queryType: AzureQueryType.AzureHealthModels,
      subscription: 'sub-a',
    });
    query.azureHealthModels = {
      healthModelResourceId: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-1',
    };

    render(<HealthModelsQueryEditor query={query} datasource={datasource} onChange={onChange} setError={jest.fn()} />);

    await waitFor(() => expect(getHealthModels).toHaveBeenCalledWith('sub-a'));
    const queryTypeSelect = await screen.findByRole('combobox', { name: /Health Models query type/i });
    await selectOptionInTest(queryTypeSelect, 'Entity Health History');

    await waitFor(() =>
      expect(onChange).toHaveBeenCalledWith(
        expect.objectContaining({
          queryType: AzureQueryType.AzureHealthModels,
          azureHealthModels: expect.objectContaining({
            healthModelResourceId: '/subscriptions/sub-a/providers/Microsoft.CloudHealth/healthmodels/hm-1',
            queryType: AzureHealthModelsQueryType.EntityHealthHistory,
          }),
        })
      )
    );
  });
});
