// SPDX-License-Identifier: Apache-2.0
import { test, expect } from '@playwright/test';

// A fixed, valid-format token (prefix + 32+ bytes of base64url).
export const TOKEN = process.env.E2E_TOKEN;
const api = (svc, m) => `http://127.0.0.1:18080/buf.registry.${svc}/${m}`;
async function call(svc, m, body, ok409 = false) {
  const r = await fetch(api(svc, m), { method: 'POST', body: JSON.stringify(body),
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${TOKEN}` } });
  if (ok409 && r.status === 409) return {};
  if (!r.ok) throw new Error(`${m}: ${r.status} ${await r.text()}`);
  return r.json();
}
const b64 = (s) => Buffer.from(s).toString('base64');

test.beforeAll(async () => {
  await call('owner.v1.OrganizationService', 'GetOrganizations', { organizationRefs: [{ name: 'acme' }] });
  await call('module.v1.ModuleService', 'CreateModules', { values: [{ ownerRef: { name: 'acme' }, name: 'weather', visibility: 'MODULE_VISIBILITY_PUBLIC' }] }, true);
  await call('module.v1.UploadService', 'Upload', { contents: [{
    moduleRef: { name: { owner: 'acme', module: 'weather' } },
    files: [{ path: 'acme/weather/v1/weather.proto', content: b64('syntax = "proto3";\npackage acme.weather.v1;\nmessage Forecast {}\n') }],
  }] });
});

async function login(page) {
  await page.goto('/#/login');
  await page.getByLabel('Token').fill(TOKEN);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('heading', { name: 'Modules' })).toBeVisible();
}

test('browse module, commits, labels and files', async ({ page }) => {
  await login(page);
  await page.getByRole('link', { name: 'acme/weather' }).click();
  await expect(page.getByRole('heading', { name: 'acme/weather' })).toBeVisible();
  await expect(page.getByRole('table', { name: 'Labels' })).toContainText('main');
  await page.getByRole('link', { name: 'Commits' }).click();
  await expect(page.getByRole('table', { name: 'Commits' })).toContainText('b5:');
  await page.getByRole('link', { name: 'Labels' }).click();
  await expect(page.getByRole('table', { name: 'Labels' })).toContainText('main');
  await page.getByRole('link', { name: 'Files' }).click();
  await expect(page.locator('pre')).toContainText('message Forecast');
});

test('manage tokens', async ({ page }) => {
  await login(page);
  await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Tokens' }).click();
  await page.getByLabel('Name').fill('ci-token');
  await page.getByRole('button', { name: 'Create token' }).click();
  await expect(page.locator('#new-secret')).toContainText('pdx_');
  page.once('dialog', (d) => d.accept());
  await page.getByRole('button', { name: 'Delete token ci-token' }).click();
  await expect(page.getByRole('button', { name: 'Delete token ci-token' })).toHaveCount(0);
});

test('unauthenticated users are sent to sign in; theme toggles', async ({ page }) => {
  await page.goto('/#/tokens');
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  await page.getByRole('button', { name: 'Toggle light/dark theme' }).click();
  await expect(page.locator('html')).toHaveAttribute('data-theme', /light|dark/);
});
