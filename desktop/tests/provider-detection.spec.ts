import { test, expect } from "@playwright/test";

/**
 * Provider detection tests.
 *
 * These tests verify that when an account is configured pointing to our
 * DDMail server (mail.letotam.ru), the client auto-detects native mode
 * and uses HTTP/2+WS instead of IMAP.
 *
 * Prerequisites:
 *   - `npm run dev` or `cargo tauri dev` running
 *   - DDMail server running at mail.letotam.ru
 */

test.describe("Provider auto-detection", () => {
  test("detect_server returns DDMail info for our server", async ({ page }) => {
    await page.goto("/");

    // Call detect_server via Tauri invoke from the browser context
    const result = await page.evaluate(async () => {
      const { invoke } = await import("@tauri-apps/api/core");
      return invoke("detect_server", { host: "mail.letotam.ru" });
    });

    expect(result).not.toBeNull();
    expect(result).toHaveProperty("server_url");
    expect(result.server_url).toContain("mail.letotam.ru");
    expect(result).toHaveProperty("api_base", "/api/desktop/v1");
    expect(result).toHaveProperty("ws_path", "/api/desktop/v1/ws");
  });

  test("detect_server returns null for third-party IMAP", async ({ page }) => {
    await page.goto("/");

    const result = await page.evaluate(async () => {
      const { invoke } = await import("@tauri-apps/api/core");
      return invoke("detect_server", { host: "imap.gmail.com" });
    });

    expect(result).toBeNull();
  });

  test("native_login returns JWT token", async ({ page }) => {
    await page.goto("/");

    const token = await page.evaluate(async () => {
      const { invoke } = await import("@tauri-apps/api/core");
      return invoke("native_login", {
        serverUrl: "https://mail.letotam.ru",
        username: "lucky",
        password: "LuckY1901!",
      });
    });

    expect(typeof token).toBe("string");
    expect(token.length).toBeGreaterThan(20); // JWT is long
  });
});
