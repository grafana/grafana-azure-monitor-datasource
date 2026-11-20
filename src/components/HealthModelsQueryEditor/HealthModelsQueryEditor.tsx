import { useEffect, useState } from 'react';

import { LoadingState, type PanelData } from '@grafana/data';
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
  data?: PanelData;
  setError: (source: string, error: AzureMonitorErrorish | undefined) => void;
}

const SUBSCRIPTIONS_ERROR_SOURCE = 'health-models-subscriptions';
const HEALTH_MODELS_ERROR_SOURCE = 'health-models-list';
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
  data,
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

  useEffect(() => {
    if (!onQueryChange || data?.state !== LoadingState.Done || !selectedHealthModelResourceId) {
      return;
    }
    const requestTarget = data.request?.targets.find((target) => target.refId === query.refId);
    if (data.request && requestTarget?.datasource?.uid !== datasource.uid) {
      return;
    }
    const modelID = getTemplateSrv().replace(selectedHealthModelResourceId, data.request?.scopedVars);
    const subscription = getTemplateSrv().replace(selectedSubscription || '', data.request?.scopedVars);
    if (modelID.split('/')[2]?.toLowerCase() !== subscription.toLowerCase()) {
      return;
    }
    for (const frame of data.series) {
      const metadata = frame.meta?.custom;
      if (
        frame.refId !== query.refId ||
        typeof metadata?.healthModelResourceId !== 'string' ||
        metadata.healthModelResourceId.toLowerCase() !== modelID.toLowerCase() ||
        typeof metadata.dataplaneUrl !== 'string' ||
        !metadata.dataplaneUrl ||
        typeof metadata.dataplaneModel?.id !== 'string' ||
        metadata.dataplaneModel.id.toLowerCase() !== modelID.toLowerCase() ||
        typeof metadata.dataplaneModel.name !== 'string' ||
        !metadata.dataplaneModel.name ||
        (metadata.dataplaneModel.location !== undefined && typeof metadata.dataplaneModel.location !== 'string')
      ) {
        continue;
      }
      const { id, name, location } = metadata.dataplaneModel;
      const saved = query.azureHealthModels;
      if (
        saved?.dataplaneUrl !== metadata.dataplaneUrl ||
        saved.dataplaneModel?.id !== id ||
        saved.dataplaneModel?.name !== name ||
        saved.dataplaneModel?.location !== location
      ) {
        // Persist discovery without triggering another health query.
        onQueryChange({
          ...query,
          azureHealthModels: {
            ...saved,
            dataplaneUrl: metadata.dataplaneUrl,
            dataplaneModel: { id, name, location },
          },
        });
      }
      break;
    }
  }, [data, datasource, onQueryChange, query, selectedHealthModelResourceId, selectedSubscription]);
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
        dataplaneModel: undefined,
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
        dataplaneModel: undefined,
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
        dataplaneModel: undefined,
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
