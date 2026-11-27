import { useEffect, useState } from 'react';

import { getTemplateSrv } from '@grafana/runtime';
import { EditorFieldGroup, EditorRow, EditorRows } from '@grafana/plugin-ui';
import { type ComboboxOption } from '@grafana/ui';

import { AzureHealthModelsQueryType, AzureQueryType } from '../../dataquery';
import type Datasource from '../../datasource';
import { type AzureMonitorQuery } from '../../types/query';
import { type AzureMonitorErrorish, type VariableOptionGroup } from '../../types/types';

import HealthModelField from './HealthModelField';
import HealthModelsQueryTypeField from './HealthModelsQueryTypeField';
import SubscriptionField from './SubscriptionField';

interface HealthModelsQueryEditorProps {
  query: AzureMonitorQuery;
  datasource: Datasource;
  subscriptionId?: string;
  variableOptionGroup?: VariableOptionGroup;
  onChange: (newQuery: AzureMonitorQuery) => void;
  onQueryChange?: (newQuery: AzureMonitorQuery) => void;
  setError: (source: string, error: AzureMonitorErrorish | undefined) => void;
}

const SUBSCRIPTIONS_ERROR_SOURCE = 'health-models-subscriptions';
const HEALTH_MODELS_ERROR_SOURCE = 'health-models-list';
const HEALTH_MODELS_ENDPOINT_ERROR_SOURCE = 'health-models-endpoint';
const DEFAULT_HEALTH_MODELS_QUERY_TYPE = AzureHealthModelsQueryType.EntityHealthSnapshot;
const HEALTH_MODELS_QUERY_TYPES = new Set<AzureHealthModelsQueryType>([
  AzureHealthModelsQueryType.EntityHealthSnapshot,
  AzureHealthModelsQueryType.EntityHealthHistory,
]);

const HealthModelsQueryEditor = ({
  query,
  datasource,
  subscriptionId,
  variableOptionGroup,
  onChange,
  onQueryChange,
  setError,
}: HealthModelsQueryEditorProps) => {
  const [subscriptions, setSubscriptions] = useState<Array<ComboboxOption<string>>>([]);
  const [healthModels, setHealthModels] = useState<Array<ComboboxOption<string>>>([]);
  const [loadingSubscriptions, setLoadingSubscriptions] = useState(true);
  const selectedSubscription = query.subscription || subscriptionId || subscriptions[0]?.value;
  const [loadingHealthModels, setLoadingHealthModels] = useState(Boolean(selectedSubscription));
  const [healthModelsSource, setHealthModelsSource] = useState({ datasource, subscriptionId: selectedSubscription });

  // Reset before committing the new source so auto-selection cannot use stale models.
  if (healthModelsSource.datasource !== datasource || healthModelsSource.subscriptionId !== selectedSubscription) {
    setHealthModelsSource({ datasource, subscriptionId: selectedSubscription });
    setHealthModels([]);
    setLoadingHealthModels(Boolean(selectedSubscription));
  }

  const selectedHealthModelResourceId = query.azureHealthModels?.healthModelResourceId;
  const selectedHealthModelsQueryType = query.azureHealthModels?.queryType;
  const resolvedModelID = getTemplateSrv().replace(selectedHealthModelResourceId || '');
  const resolvedSubscription = getTemplateSrv().replace(selectedSubscription || '');
  const dataplaneUrl = query.azureHealthModels?.dataplaneUrl;

  useEffect(() => {
    let canceled = false;
    setError(HEALTH_MODELS_ENDPOINT_ERROR_SOURCE, undefined);
    if (
      !onQueryChange ||
      !resolvedModelID ||
      dataplaneUrl ||
      resolvedModelID.split('/')[2]?.toLowerCase() !== resolvedSubscription.toLowerCase()
    ) {
      return;
    }
    datasource.azureHealthModelsDatasource
      .getDataplaneUrl(resolvedModelID)
      .then((url) => {
        if (!canceled) {
          onQueryChange({
            ...query,
            azureHealthModels: { ...query.azureHealthModels, dataplaneUrl: url },
          });
        }
      })
      .catch((error) => {
        if (!canceled) {
          setError(HEALTH_MODELS_ENDPOINT_ERROR_SOURCE, error);
        }
      });
    return () => {
      canceled = true;
    };
  }, [dataplaneUrl, datasource, onQueryChange, query, resolvedModelID, resolvedSubscription, setError]);
  const subscriptionOptions = variableOptionGroup
    ? [
        ...subscriptions,
        ...variableOptionGroup.options.map((option) => ({
          label: option.label,
          value: option.value,
          group: variableOptionGroup.label,
        })),
      ]
    : subscriptions;

  useEffect(() => {
    let canceled = false;

    datasource
      .getSubscriptions()
      .then((results) => {
        if (!canceled) {
          setSubscriptions(
            results.map((subscription) => ({
              label: subscription.text,
              value: subscription.value,
              description: subscription.value,
            }))
          );
          setError(SUBSCRIPTIONS_ERROR_SOURCE, undefined);
        }
      })
      .catch((error) => {
        if (!canceled) {
          setError(SUBSCRIPTIONS_ERROR_SOURCE, error);
        }
      })
      .finally(() => {
        if (!canceled) {
          setLoadingSubscriptions(false);
        }
      });

    return () => {
      canceled = true;
    };
  }, [datasource, setError]);

  useEffect(() => {
    let canceled = false;

    if (!selectedSubscription) {
      setError(HEALTH_MODELS_ERROR_SOURCE, undefined);
      return;
    }

    datasource
      .getHealthModels(selectedSubscription)
      .then((results) => {
        if (!canceled) {
          setHealthModels(
            results.map((healthModel) => ({
              label: healthModel.text,
              value: healthModel.value,
              description: healthModel.value,
            }))
          );
          setError(HEALTH_MODELS_ERROR_SOURCE, undefined);
        }
      })
      .catch((error) => {
        if (!canceled) {
          setError(HEALTH_MODELS_ERROR_SOURCE, error);
        }
      })
      .finally(() => {
        if (!canceled) {
          setLoadingHealthModels(false);
        }
      });

    return () => {
      canceled = true;
    };
  }, [datasource, selectedSubscription, setError]);

  useEffect(() => {
    const topHealthModelResourceId = healthModels[0]?.value;
    if (!topHealthModelResourceId || !selectedSubscription || selectedHealthModelResourceId) {
      return;
    }

    onChange({
      ...query,
      subscription: selectedSubscription,
      azureHealthModels: {
        ...query.azureHealthModels,
        healthModelResourceId: topHealthModelResourceId,
        dataplaneUrl: undefined,
        queryType: query.azureHealthModels?.queryType ?? DEFAULT_HEALTH_MODELS_QUERY_TYPE,
      },
    });
  }, [healthModels, onChange, query, selectedHealthModelResourceId, selectedSubscription]);

  useEffect(() => {
    if (!selectedHealthModelResourceId || selectedHealthModelsQueryType) {
      return;
    }

    onChange({
      ...query,
      subscription: selectedSubscription,
      queryType: AzureQueryType.AzureHealthModels,
      azureHealthModels: {
        ...query.azureHealthModels,
        healthModelResourceId: selectedHealthModelResourceId,
        queryType: DEFAULT_HEALTH_MODELS_QUERY_TYPE,
      },
    });
  }, [onChange, query, selectedHealthModelResourceId, selectedHealthModelsQueryType, selectedSubscription]);

  const onSubscriptionChange = (selection: ComboboxOption<string>) => {
    onChange({
      ...query,
      subscription: selection.value,
      azureHealthModels: {
        ...query.azureHealthModels,
        healthModelResourceId: undefined,
        dataplaneUrl: undefined,
      },
    });
  };

  const onHealthModelChange = (selection: ComboboxOption<string>) => {
    onChange({
      ...query,
      subscription: selectedSubscription,
      queryType: AzureQueryType.AzureHealthModels,
      azureHealthModels: {
        ...query.azureHealthModels,
        healthModelResourceId: selection.value,
        dataplaneUrl: undefined,
        queryType: query.azureHealthModels?.queryType ?? DEFAULT_HEALTH_MODELS_QUERY_TYPE,
      },
    });
  };

  const onQueryTypeChange = (selection: ComboboxOption<AzureHealthModelsQueryType>) => {
    if (!selection.value || !HEALTH_MODELS_QUERY_TYPES.has(selection.value)) {
      return;
    }

    onChange({
      ...query,
      subscription: selectedSubscription,
      queryType: AzureQueryType.AzureHealthModels,
      azureHealthModels: {
        ...query.azureHealthModels,
        healthModelResourceId: selectedHealthModelResourceId,
        queryType: selection.value,
      },
    });
  };

  return (
    <EditorRows>
      <EditorRow>
        <EditorFieldGroup>
          <SubscriptionField
            subscriptions={subscriptionOptions}
            selectedSubscription={selectedSubscription}
            loadingSubscriptions={loadingSubscriptions}
            onSubscriptionChange={onSubscriptionChange}
          />
          <HealthModelField
            healthModels={healthModels}
            selectedHealthModelResourceId={selectedHealthModelResourceId}
            loadingHealthModels={loadingHealthModels}
            hasSelectedSubscription={Boolean(selectedSubscription)}
            onHealthModelChange={onHealthModelChange}
          />
          <HealthModelsQueryTypeField
            selectedHealthModelsQueryType={selectedHealthModelsQueryType}
            disabled={!selectedHealthModelResourceId}
            onQueryTypeChange={onQueryTypeChange}
          />
        </EditorFieldGroup>
      </EditorRow>
    </EditorRows>
  );
};

export default HealthModelsQueryEditor;
