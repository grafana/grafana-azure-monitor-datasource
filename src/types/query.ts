import {
  type AzureHealthModelsQuery as AzureHealthModelsQueryBase,
  type AzureMonitorQuery as AzureMonitorQueryBase,
  type AzureQueryType,
} from '../dataquery';

export interface HealthModelMetadata {
  id: string;
  name: string;
  location?: string;
}

export interface AzureHealthModelsQuery extends AzureHealthModelsQueryBase {
  dataplaneUrl?: string;
  dataplaneModel?: HealthModelMetadata;
}

/**
 * Represents the query as it moves through the frontend query editor and datasource files.
 * It can represent new queries that are still being edited, so all properties are optional
 */
// TODO: This is a workaround until the type extensions issue is resolved in CUE
export interface AzureMonitorQuery extends AzureMonitorQueryBase {
  queryType?: AzureQueryType;
  azureHealthModels?: AzureHealthModelsQuery;
}
