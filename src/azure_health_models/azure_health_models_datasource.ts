import { defer, finalize, from, map, type Observable } from 'rxjs';

import { DataSourceWithBackend, getTemplateSrv, toDataQueryError, type TemplateSrv } from '@grafana/runtime';
import {
  type DataFrame,
  type DataQueryRequest,
  type DataQueryResponse,
  FieldType,
  LoadingState,
  type ScopedVars,
  toDataFrame,
} from '@grafana/data';

import { AzureHealthModelsQueryType } from '../dataquery';
import { type AzureMonitorQuery } from '../types/query';
import {
  type AzureAPIResponse,
  type AzureMonitorDataSourceInstanceSettings,
  type AzureMonitorDataSourceJsonData,
} from '../types/types';
import { fetchAllArmPages, MAX_ARM_PAGES, routeNames } from '../utils/common';
import messageFromError from '../utils/messageFromError';

interface HealthModelResource {
  id: string;
  name: string;
}

interface HealthModel extends HealthModelResource {
  location?: string;
  properties?: { dataPlaneUri?: string };
}

interface Entity extends HealthModelResource {
  properties: Record<string, unknown>;
}

interface Relationship extends HealthModelResource {
  properties: { parentEntityName: string; childEntityName: string };
}

interface Snapshot extends Record<string, unknown> {
  entityName: string;
  healthState: string;
}

interface Transition extends Record<string, unknown> {
  previousState: string;
  newState: string;
  occurredAt: string;
}

interface EntityResult {
  entityName: string;
  snapshot?: Snapshot;
  transitions?: Transition[];
  error?: string;
}

// Model selection uses the deployed API; endpoint discovery requires the preview contract.
const HEALTH_MODELS_API_VERSION = '2026-11-01';
const HEALTH_MODELS_DISCOVERY_API_VERSION = '2026-12-01-preview';
const HEALTH_MODELS_DATA_PLANE_API_VERSION = '2026-10-01-preview';
const MAX_CONCURRENT_READS = 10;
const MODEL_ID_PATTERN =
  /^\/subscriptions\/[^/%?#]+\/resourceGroups\/[^/%?#]+\/providers\/Microsoft\.CloudHealth\/healthmodels\/[^/%?#]+$/i;
const ENTITY_NAME_PATTERN = /^[a-zA-Z0-9][a-zA-Z0-9-]{1,258}[a-zA-Z0-9]$/;

const jsonFrame = (name: string, refId: string, values: unknown[]): DataFrame =>
  toDataFrame({ name, refId, fields: [{ name: 'data', type: FieldType.other, values }] });

const toHealthModelsQueryError = (error: unknown) => {
  const queryError = toDataQueryError(error instanceof Error ? error.message : error);
  return { ...queryError, message: messageFromError(queryError) || 'Health Models request failed' };
};

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

  query(request: DataQueryRequest<AzureMonitorQuery>): Observable<DataQueryResponse> {
    return defer(() => {
      const controller = new AbortController();
      const targets = request.targets.filter((target) => this.filterQuery(target));
      return from(
        Promise.all(
          targets.map((target) =>
            this.executeQuery(this.applyTemplateVariables(target, request.scopedVars), request, controller.signal)
          )
        )
      ).pipe(
        map((responses) => {
          const errors = responses.flatMap((response) => response.errors ?? []);
          return {
            data: responses.flatMap((response) => response.data),
            state: errors.length ? LoadingState.Error : LoadingState.Done,
            ...(errors.length ? { errors } : {}),
          };
        }),
        finalize(() => controller.abort())
      );
    });
  }

  private async executeQuery(
    query: AzureMonitorQuery,
    request: DataQueryRequest<AzureMonitorQuery>,
    signal: AbortSignal
  ): Promise<DataQueryResponse> {
    try {
      const modelID = query.azureHealthModels?.healthModelResourceId;
      const queryType = query.azureHealthModels?.queryType;
      if (
        !modelID ||
        !MODEL_ID_PATTERN.test(modelID) ||
        modelID.split('/').some((part) => part === '.' || part === '..')
      ) {
        throw new Error('healthModelResourceId must be a full Health Model ARM resource ID');
      }
      if (query.subscription && modelID.split('/')[2].toLowerCase() !== query.subscription.toLowerCase()) {
        throw new Error('The selected Health Model does not belong to the selected subscription');
      }
      const history = queryType === AzureHealthModelsQueryType.EntityHealthHistory;
      if (!history && queryType !== AzureHealthModelsQueryType.EntityHealthSnapshot) {
        throw new Error(`Unsupported Health Models query type: ${queryType}`);
      }
      if (!request.range?.to?.isValid()) {
        throw new Error('Health Models requires a query time range');
      }
      if (history && (!request.range.from?.isValid() || !request.range.from.isBefore(request.range.to))) {
        throw new Error('Health Models history requires a time range with from before to');
      }
      const timestamp = request.range.to.toISOString();
      const armParams = new URLSearchParams({
        'api-version': HEALTH_MODELS_DISCOVERY_API_VERSION,
        timestamp,
      });
      const model = await this.getResource<HealthModel>(`${this.resourcePath}${modelID}?${armParams}`, undefined, {
        abortSignal: signal,
        showErrorAlert: false,
      });
      if (model?.id?.toLowerCase() !== modelID.toLowerCase() || !model.name) {
        throw new Error('Health Model metadata is missing its expected id or name');
      }
      const endpoint = query.azureHealthModels?.dataplaneUrl || model.properties?.dataPlaneUri;
      if (typeof endpoint !== 'string' || !endpoint) {
        throw new Error('The Health Model metadata does not contain properties.dataPlaneUri');
      }
      const entities = await this.getArmPages<Entity>(`${modelID}/entities?${armParams}`, signal);
      const seen = new Set<string>();
      for (const entity of entities) {
        if (!entity?.id || !ENTITY_NAME_PATTERN.test(entity.name) || !entity.properties || seen.has(entity.name)) {
          throw new Error('The entity list contains a malformed or duplicate entity');
        }
        seen.add(entity.name);
      }
      const relationships = await this.getArmPages<Relationship>(`${modelID}/relationships?${armParams}`, signal);
      if (
        relationships.some(
          (relationship) =>
            !relationship?.id ||
            !relationship.name ||
            !relationship.properties?.parentEntityName ||
            !relationship.properties.childEntityName
        )
      ) {
        throw new Error('The relationship list contains a malformed relationship');
      }
      const results: EntityResult[] = [];
      for (let offset = 0; offset < entities.length; offset += MAX_CONCURRENT_READS) {
        signal.throwIfAborted();
        results.push(
          ...(await Promise.all(
            entities.slice(offset, offset + MAX_CONCURRENT_READS).map(async ({ name }): Promise<EntityResult> => {
              try {
                if (history) {
                  const transitions = await this.getEntityHealthHistory(
                    endpoint,
                    name,
                    request.range.from.toISOString(),
                    timestamp,
                    signal
                  );
                  if (
                    transitions.some(
                      (transition) =>
                        !transition?.previousState ||
                        !transition.newState ||
                        typeof transition.occurredAt !== 'string' ||
                        !Number.isFinite(Date.parse(transition.occurredAt))
                    )
                  ) {
                    throw new Error('Health Models history contains a malformed transition');
                  }
                  return { entityName: name, transitions };
                }
                const snapshot = await this.getEntityHealth(endpoint, name, timestamp, signal);
                if (snapshot?.entityName !== name || !snapshot.healthState) {
                  throw new Error('The snapshot is missing its expected entityName or healthState');
                }
                return { entityName: name, snapshot };
              } catch (error) {
                signal.throwIfAborted();
                return {
                  entityName: name,
                  error: toHealthModelsQueryError(error).message,
                };
              }
            })
          ))
        );
      }
      const resultFrame = jsonFrame(history ? 'transitions' : 'snapshots', query.refId, results);
      const failed = results.filter((result) => result.error);
      if (failed.length) {
        resultFrame.meta = {
          notices: [
            {
              severity: 'warning',
              text: `${failed.length} of ${results.length} Health Model entity reads failed; see each entity's error field`,
            },
          ],
        };
      }
      const data = [
        jsonFrame('healthModel', query.refId, [model]),
        jsonFrame('entities', query.refId, entities),
        jsonFrame('relationships', query.refId, relationships),
        resultFrame,
      ];
      if (failed.length && failed.length === results.length) {
        return {
          data,
          errors: [
            {
              refId: query.refId,
              message: `All ${failed.length} Health Model entity reads failed; first failure for ${failed[0].entityName}: ${failed[0].error}`,
            },
          ],
        };
      }
      return { data };
    } catch (error) {
      return {
        data: [],
        errors: [{ ...toHealthModelsQueryError(error), refId: query.refId }],
      };
    }
  }

  getEntityHealth(dataplaneUrl: string, entity: string, timestamp: string, signal?: AbortSignal): Promise<Snapshot> {
    return this.getResource<Snapshot>(
      routeNames.healthModels,
      {
        dataplaneUrl,
        path: `api/entities/${entity}`,
        'api-version': HEALTH_MODELS_DATA_PLANE_API_VERSION,
        timestamp,
      },
      { abortSignal: signal, showErrorAlert: false }
    );
  }

  getEntityHealthHistory(
    dataplaneUrl: string,
    entity: string,
    startAt: string,
    endAt: string,
    signal?: AbortSignal
  ): Promise<Transition[]> {
    const endpoint = new URL(dataplaneUrl);
    const url = new URL(`${endpoint.pathname.replace(/\/+$/, '')}/api/entities/${entity}/transitions`, endpoint);
    url.search = new URLSearchParams({
      'api-version': HEALTH_MODELS_DATA_PLANE_API_VERSION,
      startAt,
      endAt,
    }).toString();
    return this.getPages<Transition>(routeNames.healthModels, url, (pageURL) =>
      this.getResource<AzureAPIResponse<Transition>>(
        routeNames.healthModels,
        {
          dataplaneUrl,
          path: `${pageURL.pathname.slice(endpoint.pathname.replace(/\/+$/, '').length)}${pageURL.search}`,
        },
        { abortSignal: signal, showErrorAlert: false }
      )
    );
  }

  private getArmPages<T>(path: string, signal?: AbortSignal): Promise<T[]> {
    // ARM hosts from nextLink are never used as targets; the existing proxy uses the configured ARM service.
    const url = new URL(path, 'https://management.azure.com');
    return this.getPages<T>(this.resourcePath, url, (pageURL) =>
      this.getResource<AzureAPIResponse<T>>(`${this.resourcePath}${pageURL.pathname}${pageURL.search}`, undefined, {
        abortSignal: signal,
        showErrorAlert: false,
      })
    );
  }

  private getPages<T>(
    prefix: string,
    initialURL: URL,
    fetchPage: (url: URL) => Promise<AzureAPIResponse<T>>
  ): Promise<T[]> {
    const seen = new Set<string>();
    return fetchAllArmPages<T>(prefix, `${prefix}${initialURL.pathname}${initialURL.search}`, async (path) => {
      if (seen.has(path)) {
        throw new Error('Health Models pagination repeated a page');
      }
      seen.add(path);
      const page = await fetchPage(new URL(path.slice(prefix.length), initialURL));
      if (!Array.isArray(page?.value)) {
        throw new Error('Health Models list response must contain a value array');
      }
      if (!page.nextLink) {
        return page;
      }
      const next = new URL(page.nextLink, initialURL);
      if (
        next.protocol !== 'https:' ||
        next.username ||
        next.password ||
        next.hash ||
        next.pathname !== initialURL.pathname ||
        (prefix === routeNames.healthModels && next.origin !== initialURL.origin)
      ) {
        throw new Error('Health Models nextLink must stay on the original endpoint and operation');
      }
      if (seen.size >= MAX_ARM_PAGES) {
        throw new Error(`Health Models pagination exceeded ${MAX_ARM_PAGES} pages`);
      }
      return { ...page, nextLink: next.toString() };
    });
  }

  applyTemplateVariables(query: AzureMonitorQuery, scopedVars: ScopedVars): AzureMonitorQuery {
    const originalSubscription = query.subscription || this.defaultSubscriptionId || '';
    const subscription = this.templateSrv.replace(originalSubscription, scopedVars);
    const original = query.azureHealthModels?.healthModelResourceId;
    const healthModelResourceId = original ? this.templateSrv.replace(original, scopedVars) : original;
    const subscriptionInModel = healthModelResourceId?.split('/')[2];
    // Resolve variable-backed endpoints on each execution rather than cache variable values.
    const changed =
      healthModelResourceId !== original ||
      subscription !== originalSubscription ||
      (subscriptionInModel && subscriptionInModel.toLowerCase() !== subscription.toLowerCase());
    return {
      ...query,
      subscription,
      azureHealthModels: {
        ...query.azureHealthModels,
        healthModelResourceId,
        dataplaneUrl: changed ? undefined : query.azureHealthModels?.dataplaneUrl,
      },
    };
  }

  async getDataplaneUrl(modelResourceId: string): Promise<string> {
    const model = await this.getResource<HealthModel>(
      `${this.resourcePath}${modelResourceId}?api-version=${HEALTH_MODELS_DISCOVERY_API_VERSION}`
    );
    if (typeof model.properties?.dataPlaneUri !== 'string' || !model.properties.dataPlaneUri) {
      throw new Error('The Health Model metadata does not contain properties.dataPlaneUri');
    }
    return model.properties.dataPlaneUri;
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
