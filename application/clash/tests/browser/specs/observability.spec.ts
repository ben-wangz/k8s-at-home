import { test, expect, connect, select, traffic, real } from './support';

test('connections.snapshot', async ({ page }) => {
  await connect(page);
  await select(page, 'Node A');
  await page.getByRole('link', { name: 'Connections', exact: true }).click();
  const source = traffic();
  try {
    await expect(page.locator('#connection-status')).toContainText('Last update:');
    await expect(page.locator('#connection-rows')).toContainText('Node A');
    await expect(page.locator('#connection-count')).not.toHaveText('0 active');
    await page.getByLabel('Filter connections').fill('no-matching-destination');
    await expect(page.locator('#connection-rows')).toContainText('No matching active connections.');
  } finally { source.close(); }
});

test('traffic.rates-and-totals', async ({ page }) => {
  await connect(page);
  await select(page, 'Node A');
  const source = traffic();
  try {
    await expect(page.locator('#download-rate')).not.toHaveText('Unavailable');
    await expect.poll(async () => await page.locator('#download-rate').textContent()).not.toBe('0 B/s');
    await expect(page.locator('#download-total')).not.toHaveText('Unavailable');
    await expect(page.locator('#traffic-status')).toContainText('Live traffic');
  } finally { source.close(); }
});

test('connection-metadata.optional-fields', async ({ page, request }) => {
  await request.post('http://127.0.0.1:43819/__fixture', { data: { missingMetadata: true } });
  await connect(page);
  await page.getByRole('link', { name: 'Connections', exact: true }).click();
  await expect(page.locator('#connection-rows')).toContainText('Unavailable');
  await expect(page.locator('#connection-count')).toHaveText('1 active');
  await expect(page.locator('#connection-rows')).toContainText('Node A');
  await request.post('http://127.0.0.1:43819/__fixture', { data: { emptyConnections: true } });
  await expect(page.locator('#connection-count')).toHaveText('0 active');
  await expect(page.locator('#connection-rows')).toContainText('No matching active connections.');
});

test('live-logs.events', async ({ page }) => {
  await connect(page);
  await select(page, 'Node A');
  await page.getByRole('link', { name: 'Live logs', exact: true }).click();
  await expect(page.locator('#log-status')).toHaveText('Connected');
  const source = traffic();
  try {
    await expect(page.locator('.log-entry').first()).toBeVisible();
    if (real) await expect(page.locator('#log-entries')).toContainText(/TCP|HTTP|8080/i);
    await page.getByLabel('Filter logs').fill('unmatched-log-marker');
    await expect(page.locator('.log-entry')).toHaveCount(0);
    await page.getByLabel('Filter logs').fill('');
    await expect(page.locator('.log-entry').first()).toBeVisible();
  } finally { source.close(); }
});

test('live-logs.controls', async ({ page }) => {
  await connect(page);
  await page.getByRole('link', { name: 'Live logs', exact: true }).click();
  await expect(page.locator('#log-status')).toHaveText('Connected');
  const source = traffic();
  try {
    await expect(page.locator('.log-entry').first()).toBeVisible();
    await page.getByRole('button', { name: 'Pause display', exact: true }).click();
    await expect(page.getByRole('button', { name: 'Resume display', exact: true })).toHaveAttribute('aria-pressed', 'true');
    await page.getByRole('button', { name: 'Clear buffer', exact: true }).click();
    await expect(page.locator('.log-entry')).toHaveCount(0);
    await page.getByRole('button', { name: 'Resume display', exact: true }).click();
    await page.getByLabel('Log level', { exact: true }).selectOption('debug');
    await expect(page.locator('#log-status')).toHaveText('Connected');
    await page.getByLabel('Auto-scroll', { exact: true }).uncheck();
    await expect(page.getByLabel('Auto-scroll', { exact: true })).not.toBeChecked();
  } finally { source.close(); }
});

test('log-buffer.capacity', async ({ page, request }) => {
  await connect(page);
  await page.getByRole('link', { name: 'Live logs', exact: true }).click();
  await expect(page.locator('#log-status')).toHaveText('Connected');
  await page.getByRole('button', { name: 'Pause display', exact: true }).click();
  const before = await page.locator('#log-entries').textContent();
  await request.post('http://127.0.0.1:43819/__fixture', { data: { burst: 1200 } });
  await expect(page.locator('#log-entries')).toHaveText(before || '');
  await page.getByRole('button', { name: 'Resume display', exact: true }).click();
  await expect(page.locator('#log-count')).toHaveText('1000 / 1000 events');
  await expect(page.locator('.log-entry')).toHaveCount(1000);
  await expect(page.locator('#log-entries')).toContainText('fixture event 1199');
  await expect(page.locator('.log-entry').first()).toHaveText(/fixture event 200$/);
  await page.getByLabel('Log level', { exact: true }).selectOption('error');
  await expect(page.locator('#log-status')).toHaveText('Connected');
  await page.getByRole('button', { name: 'Clear buffer', exact: true }).click();
  await request.post('http://127.0.0.1:43819/__fixture', { data: { burst: 2, level: 'info' } });
  await request.post('http://127.0.0.1:43819/__fixture', { data: { burst: 1, level: 'error' } });
  await expect(page.locator('.log-entry')).toHaveCount(1);
  await expect(page.locator('.log-level')).toHaveText('error');
});
