import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./tests",
  timeout: 30_000,
  retries: 0,
  use: {
    // Tauri WebView connects on this URL when run in dev mode
    baseURL: "http://localhost:1420",
    trace: "on-first-retry",
  },
  // Dev server — `npm run dev` must be running separately,
  // or uncomment webServer below to auto-start it.
  // webServer: {
  //   command: "npm run dev",
  //   url: "http://localhost:1420",
  //   reuseExistingServer: true,
  // },
});
