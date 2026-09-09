const { defineConfig } = require('@playwright/test');
module.exports = defineConfig({
  testDir: './tests',
  use: { baseURL: 'http://127.0.0.1:4177', browserName: 'chromium' },
  webServer: {
    command: 'python3 -m http.server 4177 --bind 127.0.0.1',
    url: 'http://127.0.0.1:4177',
    reuseExistingServer: !process.env.CI,
  },
});
