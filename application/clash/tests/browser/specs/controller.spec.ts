import { promisify } from 'node:util';
import { execFile } from 'node:child_process';
import { test, expect, connect, group, real, run, secret, traffic } from './support';

test('installation.assets-and-reload', async ({ page }) => {
  const failed: string[] = [];
  page.on('pageerror', error => failed.push(error.message));
  await connect(page);
  await expect(page).toHaveTitle('Clash Console');
  await page.getByRole('link', { name: 'Connections', exact: true }).click();
  await page.reload();
  if (secret) {
    await page.locator('#controller-settings').evaluate((element: HTMLDetailsElement) => { element.open = true; });
    await page.getByLabel('Controller secret').fill(secret);
    await page.getByRole('button', { name: 'Connect', exact: true }).click();
  }
  await expect(page.getByRole('heading', { name: 'Connections', exact: true })).toBeVisible();
  await expect(page.locator('#connection-status')).toContainText('Connected');
  expect(await page.evaluate(() => ({ local: localStorage.length, session: sessionStorage.length }))).toEqual({ local: 0, session: 0 });
  expect(failed).toEqual([]);
});

test('controller-auth.http-and-streams', async ({ page, request }) => {
  const credential = real ? secret : 'fixture-secret';
  if (!real) await request.post('http://127.0.0.1:43819/__fixture', { data: { secret: credential } });
  await page.goto('');
  await expect(page.locator('#controller-status')).toHaveText('Authentication failed');
  await page.getByLabel('Controller secret').fill('invalid-controller-secret');
  await page.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect(page.locator('#controller-status')).toHaveText('Authentication failed');
  await page.getByLabel('Controller secret').fill(credential);
  await page.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect(page.locator('#controller-status')).toHaveText('Connected');
  await page.getByRole('link', { name: 'Connections', exact: true }).click();
  await expect(page.locator('#connection-status')).toContainText('Last update:');
  await page.getByRole('link', { name: 'Live logs', exact: true }).click();
  await expect(page.locator('#log-status')).toHaveText('Connected');
  const source = traffic();
  try { await expect(page.locator('.log-entry').first()).toBeVisible(); } finally { source.close(); }
  expect(await page.evaluate(() => localStorage.length + sessionStorage.length)).toBe(0);
});

test('stream-recovery.lifecycle', async ({ page, request }) => {
  await connect(page);
  await page.getByRole('link', { name: 'Connections', exact: true }).click();
  await expect(page.locator('#connection-status')).toContainText('Last update:');
  await request.post('http://127.0.0.1:43819/__fixture', { data: { outage: true } });
  await expect(page.locator('#connection-status')).toContainText('stale');
  await expect(page.locator('#connection-rows')).toContainText('traffic.fixture');
  await request.post('http://127.0.0.1:43819/__fixture', { data: { outage: false } });
  await expect(page.locator('#connection-status')).toContainText('Connected');
  await expect.poll(async () => (await (await request.get('http://127.0.0.1:43819/__fixture')).json()).counts).toEqual({ '/traffic': 1, '/connections': 1 });
  await page.getByRole('link', { name: 'Live logs', exact: true }).click();
  await expect.poll(async () => (await (await request.get('http://127.0.0.1:43819/__fixture')).json()).counts).toEqual({ '/traffic': 1, '/logs': 1 });
  await page.locator('#controller-settings').evaluate((element: HTMLDetailsElement) => { element.open = true; });
  await page.getByLabel('Controller URL').fill('http://127.0.0.1:43819/other/');
  await page.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect(page.locator('#controller-status')).toHaveText('Connected');
  await expect.poll(async () => (await (await request.get('http://127.0.0.1:43819/__fixture')).json()).counts).toEqual({ '/traffic': 1, '/logs': 1 });
});

test('controller-recovery.outage', async ({ page, request }) => {
  test.setTimeout(240000);
  await connect(page);
  await page.getByRole('link', { name: 'Connections', exact: true }).click();
  await expect(page.locator('#connection-status')).toContainText('Last update:');
  if (!real) {
    await request.post('http://127.0.0.1:43819/__fixture', { data: { outage: true } });
    await expect(page.locator('#connection-status')).toContainText('stale');
    await request.post('http://127.0.0.1:43819/__fixture', { data: { outage: false } });
  } else {
    const command = promisify(execFile);
    const args = ['--context', run.kubeContext, '--namespace', run.namespace];
    try {
      await command('kubectl', [...args, 'scale', `deployment/${run.deployment}`, '--replicas=0']);
      await expect(page.locator('#connection-status')).toContainText('stale', { timeout: 45000 });
    } finally { await command('kubectl', [...args, 'scale', `deployment/${run.deployment}`, '--replicas=1']); }
    await command('kubectl', [...args, 'rollout', 'status', `deployment/${run.deployment}`, '--timeout=180s']);
  }
  await expect(page.locator('#connection-status')).toContainText('Connected', { timeout: 45000 });
});

test('ingress-access.secure-route', async ({ page }) => {
  await connect(page);
  if (real) expect(new URL(page.url()).protocol).toBe('https:');
  await page.getByRole('link', { name: 'Connections', exact: true }).click();
  await expect(page.locator('#connection-status')).toContainText('Last update:');
  await page.getByRole('link', { name: 'Live logs', exact: true }).click();
  await expect(page.locator('#log-status')).toHaveText('Connected');
});
