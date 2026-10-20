import { defineConfig } from '@playwright/test';
import { existsSync, readFileSync } from 'node:fs';

const path = process.env.CLASH_TEST_RUN_CONTEXT;
const run = path && existsSync(path) ? JSON.parse(readFileSync(path, 'utf8')) : { mode: 'mock' };
const real = run.mode === 'integration';

export default defineConfig({
  testDir: './specs',
  workers: 1,
  fullyParallel: false,
  retries: 0,
  timeout: real ? 120000 : 30000,
  expect: { timeout: real ? 30000 : 7000 },
  outputDir: process.env.CLASH_TEST_OUTPUT_DIR || './test-results',
  reporter: 'list',
  use: {
    baseURL: real ? run.url : 'http://127.0.0.1:43819/ui/',
    browserName: 'chromium',
    launchOptions: { executablePath: process.env.CLASH_TEST_BROWSER_EXECUTABLE },
    headless: true,
    trace: 'off',
    video: 'off',
    screenshot: 'only-on-failure',
  },
  webServer: real ? undefined : {
    command: 'node ../fixtures/controller/server.mjs',
    url: 'http://127.0.0.1:43819/version',
    reuseExistingServer: false,
    timeout: 20000,
  },
});
