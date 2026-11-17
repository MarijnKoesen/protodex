// SPDX-License-Identifier: Apache-2.0
import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.js',
  use: { baseURL: 'http://127.0.0.1:18080' },
  webServer: {
    command: './start.sh',
    url: 'http://127.0.0.1:18080/healthz',
    reuseExistingServer: false,
    timeout: 60_000,
  },
});
