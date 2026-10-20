import { test, expect, connect, group, select, controllerGet, real } from './support';

test('proxy-inventory.members', async ({ page }) => {
  await connect(page);
  await expect(group(page).getByRole('button', { name: 'Select Node A in PROXY', exact: true })).toBeVisible();
  await expect(group(page).getByRole('button', { name: 'Select Node / B in PROXY', exact: true })).toBeVisible();
  await expect(group(page).getByRole('button', { name: 'Select DIRECT in PROXY', exact: true })).toBeVisible();
  await page.getByLabel('Filter proxies').fill('Node / B');
  await expect(group(page).getByRole('button', { name: 'Select Node A in PROXY', exact: true })).toHaveCount(0);
  await expect(group(page).getByRole('button', { name: 'Select Node / B in PROXY', exact: true })).toBeVisible();
});

test('proxy-selection.readback', async ({ page }) => {
  await connect(page);
  await select(page, 'Node A');
  await select(page, 'Node / B');
  expect((await controllerGet(page, '/proxies/PROXY')).now).toBe('Node / B');
  await connect(page);
  await expect(group(page).getByText('Current selection: Node / B', { exact: true })).toBeVisible();
});

test('selection-errors.rejection', async ({ page }) => {
  await connect(page);
  await select(page, 'Node A');
  await group(page).getByRole('button', { name: 'Select Blocked in PROXY', exact: true }).click();
  await expect(page.locator('#proxy-message')).toContainText('rejected');
  await expect(group(page).getByText('Current selection: Node A', { exact: true })).toBeVisible();
  expect((await controllerGet(page, '/proxies/PROXY')).now).toBe('Node A');
});

test('selection-errors.names-and-pending', async ({ page, request }) => {
  await connect(page);
  await request.post('http://127.0.0.1:43819/__fixture', { data: { slow: true } });
  const target = group(page, 'Group / one').getByRole('button', { name: 'Select Node / B in Group / one', exact: true });
  await target.click();
  await expect(target).toBeDisabled();
  await expect(group(page, 'Group / one').getByText('Current selection: Node / B', { exact: true })).toBeVisible();
  expect((await controllerGet(page, '/proxies/Group%20%2F%20one')).now).toBe('Node / B');
  const state = await (await request.get('http://127.0.0.1:43819/__fixture')).json();
  expect(state.writes).toBe(1);
  await expect(group(page, 'Automatic').getByRole('button').first()).toBeDisabled();
  await expect(group(page).getByRole('button', { name: 'Select Node / B in PROXY', exact: true })).toContainText('Delay unavailable');
});
