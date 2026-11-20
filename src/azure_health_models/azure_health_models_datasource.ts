import { DataSourceWithBackend, getTemplateSrv, type TemplateSrv } from '@grafana/runtime';
import { type ScopedVars } from '@grafana/data';

import { type AzureMonitorQuery } from '../types/query';
import {
  type AzureAPIResponse,
  type AzureMonitorDataSourceInstanceSettings,
  type AzureMonitorDataSourceJsonData,
} from '../types/types';
import { fetchAllArmPages, routeNames } from '../utils/common';

interface HealthModelResource {
  id: string;
  name: string;
}

// Model selection uses deployed ARM metadata; preview endpoint discovery runs in the backend.
const HEALTH_MODELS_API_VERSION = '2026-11-01';

export default class AzureHealthModelsDatasource extends DataSourceWithBackend<
  AzureMonitorQuery,
  AzureMonitorDataSourceJsonData
> {
  resourcePath: string;
  defaultSubscriptionId?: string;

  constructor(
    instanceSettings: AzureMonitorDataSourceInstanceSettings,
    private readonly templateSrv: TemplateSrv = getTemplateSrv()
  ) {
    super(instanceSettings);
    this.resourcePath = routeNames.azureMonitor;
    this.defaultSubscriptionId = instanceSettings.jsonData.subscriptionId;
  }

  filterQuery(query: AzureMonitorQuery): boolean {
    return !query.hide && Boolean(query.azureHealthModels?.healthModelResourceId && query.azureHealthModels.queryType);
  }

  applyTemplateVariables(query: AzureMonitorQuery, scopedVars: ScopedVars): AzureMonitorQuery {
    const originalSubscription = query.subscription || this.defaultSubscriptionId || '';
    const subscription = this.templateSrv.replace(originalSubscription, scopedVars);
    const original = query.azureHealthModels?.healthModelResourceId;
    const healthModelResourceId = original ? this.templateSrv.replace(original, scopedVars) : original;
    const subscriptionInModel = healthModelResourceId?.split('/')[2];
    const changed =
      query.azureHealthModels?.dataplaneModel?.id?.toLowerCase() !== healthModelResourceId?.toLowerCase() ||
      (subscriptionInModel && subscriptionInModel.toLowerCase() !== subscription.toLowerCase());
    return {
      ...query,
      subscription,
      azureHealthModels: {
        ...query.azureHealthModels,
        healthModelResourceId,
        dataplaneUrl: changed ? undefined : query.azureHealthModels?.dataplaneUrl,
        dataplaneModel: changed ? undefined : query.azureHealthModels?.dataplaneModel,
      },
    };
  }

  async getHealthModels(subscriptionId: string): Promise<Array<{ text: string; value: string }>> {
    const resolvedSubscriptionId = this.templateSrv.replace(subscriptionId || this.defaultSubscriptionId || '');
    if (!resolvedSubscriptionId) {
      return [];
    }

    const value = await fetchAllArmPages<HealthModelResource>(
      this.resourcePath,
      `${this.resourcePath}/subscriptions/${resolvedSubscriptionId}/providers/Microsoft.CloudHealth/healthmodels?api-version=${HEALTH_MODELS_API_VERSION}`,
      (path) => this.getResource<AzureAPIResponse<HealthModelResource>>(path)
    );

    return value
      .filter((model) => Boolean(model.id))
      .map((model) => ({
        text: model.name || model.id,
        value: model.id,
      }));
  }
}
