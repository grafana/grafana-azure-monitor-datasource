import { type Page } from '@playwright/test';

import { components } from '../../src/e2e/selectors';
import type { AzureMonitorQuery } from '../../src/types/query';

export const PLUGIN_TYPE = 'grafana-azure-monitor-datasource';
export const PROVISIONED_FILE = 'datasources.yml';

// GRAFANA_URL is set only by the Cloud cron workflow (playwright-cloud).
export const isCloudRun = !!process.env.GRAFANA_URL;

export const PLACEHOLDER_TENANT_ID = '00000000-0000-0000-0000-000000000001';
export const PLACEHOLDER_CLIENT_ID = '00000000-0000-0000-0000-000000000002';
export const PLACEHOLDER_SUBSCRIPTION = {
  subscriptionId: '00000000-0000-0000-0000-000000000003',
  displayName: 'E2E subscription',
};

export const placeholderJsonData = {
  azureCredentials: {
    authType: 'clientsecret',
    azureCloud: 'AzureCloud',
    tenantId: PLACEHOLDER_TENANT_ID,
    clientId: PLACEHOLDER_CLIENT_ID,
  },
  subscriptionId: PLACEHOLDER_SUBSCRIPTION.subscriptionId,
};

export const PLACEHOLDER_DATA_SOURCE = {
  type: PLUGIN_TYPE,
  jsonData: placeholderJsonData,
  secureJsonData: { clientSecret: 'placeholder-client-secret' },
};

// A Managed Identity default makes the editor request subscriptions on load, and Grafana caches the data source
// for 5 seconds, so a call right after a save still uses Managed Identity.
export const CLIENT_SECRET_DATA_SOURCE = {
  type: PLUGIN_TYPE,
  jsonData: { azureCredentials: { authType: 'clientsecret', azureCloud: 'AzureCloud' } },
};

const FROM_ISO = '2026-10-20T12:00:00.000Z';
const TO_ISO = '2026-10-20T13:00:00.000Z';

/** Builds an Explore URL that loads one query for the data source over a fixed time range. */
export const exploreUrl = (uid: string, query: Omit<AzureMonitorQuery, 'refId'> = {}) => {
  const panes = JSON.stringify({
    explore: {
      datasource: uid,
      queries: [{ refId: 'A', datasource: { type: PLUGIN_TYPE, uid }, ...query }],
      range: { from: FROM_ISO, to: TO_ISO },
    },
  });
  return `/explore?orgId=1&schemaVersion=1&panes=${encodeURIComponent(panes)}`;
};

// Cloud stacks send queries to the query service instead of /api/ds/query.
export const QUERY_DATA_URL = /\/api\/ds\/query|\/apis\/[^/]+\/v0alpha1\/namespaces\/[^/]+\/query\b/;

/** Selects App Registration (Client Secret), which is not the default when Grafana enables managed or workload identity. */
export const selectClientSecretAuth = async (page: Page) => {
  await page.getByTestId(components.configEditor.authType.select).getByRole('combobox').click();
  await page.getByRole('option', { name: 'App Registration (Client Secret)' }).click();
};

/** Selects App Registration (Client Secret) and fills its fields in the config editor. */
export const fillClientSecretCredentials = async (
  page: Page,
  tenantId: string,
  clientId: string,
  clientSecret: string
) => {
  await selectClientSecretAuth(page);
  await page.getByRole('textbox', { name: 'Tenant ID' }).fill(tenantId);
  await page.getByRole('textbox', { name: 'Client ID' }).fill(clientId);
  await page.getByRole('textbox', { name: 'Client Secret' }).fill(clientSecret);
};

// Cloud stacks can save and health-check a data source through the datasource API server instead of /api/datasources.
const dataSourcePath = (uid: string) =>
  `(?:/api/datasources/uid/|/apis/[^/]+/v0alpha1/namespaces/[^/]+/datasources/)${uid}`;
const saveUrl = (uid: string) => new RegExp(`${dataSourcePath(uid)}(?:\\?|$)`);
const healthUrl = (uid: string) => new RegExp(`${dataSourcePath(uid)}/health(?:\\?|$)`);

/** Waits for the PUT that saves the data source. */
export const waitForSave = (page: Page, uid: string) =>
  page.waitForResponse((response) => response.request().method() === 'PUT' && saveUrl(uid).test(response.url()));

/** Clicks Save & test and returns the health check response. */
export const saveAndTest = async (page: Page, uid: string) => {
  const save = waitForSave(page, uid);
  const health = page.waitForResponse((response) => healthUrl(uid).test(response.url()));
  await page.getByRole('button', { name: 'Save & test' }).click();
  await save;
  return health;
};

/** Answers the health check of the data source with the given result. */
export const mockHealthCheck = async (
  page: Page,
  uid: string,
  result: { status: 'OK' | 'ERROR'; message: string },
  status = 200
) => {
  await page.route(healthUrl(uid), (route) => route.fulfill({ json: result, status }));
};

const SUBSCRIPTIONS_URL = /\/resources\/azuremonitor\/subscriptions\b/;

/** Answers the subscriptions resource call with the placeholder subscription, or with an ARM error. */
export const mockSubscriptions = async (page: Page, status: 200 | 500 = 200) => {
  await page.route(SUBSCRIPTIONS_URL, (route) =>
    route.fulfill(
      status === 200
        ? { json: { value: [PLACEHOLDER_SUBSCRIPTION] } }
        : { status, json: { message: 'mocked subscriptions failure' } }
    )
  );
};

export interface LiveCredentials {
  tenantId: string;
  clientId: string;
  clientSecret: string;
}

export const CLOUD_VERSION_CONFLICT_REASON =
  'Save & test after Load Subscriptions fails with a version conflict on the Cloud instance until it runs a ' +
  'release with https://github.com/grafana/grafana-azure-monitor-datasource/pull/137';

export const LIVE_CREDENTIALS_SKIP_REASON =
  'Requires DS_INSTANCE_TENANT_ID, DS_INSTANCE_CLIENT_ID and DS_INSTANCE_CLIENT_SECRET';

/** Returns the app registration that the Cloud cron workflow injects, or undefined when any part is unset. */
export const readLiveCredentials = (): LiveCredentials | undefined => {
  const tenantId = process.env.DS_INSTANCE_TENANT_ID;
  const clientId = process.env.DS_INSTANCE_CLIENT_ID;
  const clientSecret = process.env.DS_INSTANCE_CLIENT_SECRET;
  return tenantId && clientId && clientSecret ? { tenantId, clientId, clientSecret } : undefined;
};
