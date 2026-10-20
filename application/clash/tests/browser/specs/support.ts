import { test as base, expect, type Page } from '@playwright/test';
import { existsSync, readFileSync } from 'node:fs';
import { request as httpRequest } from 'node:http';

const contextPath = process.env.CLASH_TEST_RUN_CONTEXT;
export const run = contextPath && existsSync(contextPath) ? JSON.parse(readFileSync(contextPath, 'utf8')) : { mode: 'mock' };
export const real = run.mode === 'integration';
export const secret = real && run.secretFile && existsSync(run.secretFile) ? readFileSync(run.secretFile, 'utf8') : '';

export const test = base.extend({
  page: async ({ page, request }, use) => {
    if (!real) await request.post('http://127.0.0.1:43819/__fixture', { data: { reset: true } });
    await use(page);
  },
});
export { expect };

test.afterEach(async ({ page, browser }, info) => {
  if (info.status !== 'passed') return;
  await info.attach('check-evidence', { body: JSON.stringify({ check: info.title, mode: real ? 'integration' : 'mock', browser: browser.version(), status: await page.locator('#controller-status').textContent().catch(() => 'unavailable') }), contentType: 'application/json' });
  await info.attach('screenshot', { body: await page.screenshot(), contentType: 'image/png' });
});

export async function connect(page: Page, credential = secret): Promise<void> {
  await page.goto('');
  if (credential) {
    await page.locator('#controller-settings').evaluate((element: HTMLDetailsElement) => { element.open = true; });
    await page.getByLabel('Controller secret').fill(credential);
    await page.getByRole('button', { name: 'Connect', exact: true }).click();
  }
  await expect(page.locator('#controller-status')).toHaveText('Connected');
  await expect(page.getByRole('article', { name: 'Proxy group PROXY' })).toBeVisible();
}

export function group(page: Page, name = 'PROXY') { return page.getByRole('article', { name: `Proxy group ${name}`, exact: true }); }

export async function select(page: Page, name: string): Promise<void> {
  const button = group(page).getByRole('button', { name: `Select ${name} in PROXY`, exact: true });
  if (await button.getAttribute('aria-pressed') !== 'true') await button.click();
  await expect(group(page).getByText(`Current selection: ${name}`, { exact: true })).toBeVisible();
}

export async function controllerGet(page: Page, path: string) {
  const base = real ? run.controllerURL : 'http://127.0.0.1:43819';
  const response = await page.request.get(base + path, { headers: secret ? { Authorization: `Bearer ${secret}` } : {} });
  expect(response.ok()).toBeTruthy();
  return response.json();
}

export function traffic(): { close(): void } {
  if (!real) return { close() {} };
  const proxy = new URL(run.proxyURL);
  const target = new URL(run.trafficURL);
  const request = httpRequest({ hostname: proxy.hostname, port: proxy.port, method: 'GET', path: target.href, headers: { Host: target.host } }, response => response.on('data', () => {}));
  request.on('error', () => {});
  request.end();
  return { close: () => request.destroy() };
}
