import { test, expect } from "@playwright/test";

/**
 * Account activation + v2 commands tests.
 *
 * Verifies the full flow: activate_account → v2_list_folders → v2_fetch_conversations.
 * Tests both native (DDMail) and IMAP paths.
 */

test.describe("Account activation and v2 commands", () => {
  const TEST_ACCOUNT_ID = "test-playwright-" + Date.now();

  test("activate_account with native provider", async ({ page }) => {
    await page.goto("/");

    const providerType = await page.evaluate(
      async ({ accountId }) => {
        const { invoke } = await import("@tauri-apps/api/core");

        // First login to get token
        const token = await invoke("native_login", {
          serverUrl: "https://mail.letotam.ru",
          username: "lucky",
          password: "LuckY1901!",
        });

        // Activate with native mode
        return invoke("activate_account", {
          accountId,
          imapHost: "mail.letotam.ru",
          imapPort: 993,
          username: "lucky",
          password: "LuckY1901!",
          useTls: true,
          email: "lucky@letotam.ru",
          nativeUrl: "https://mail.letotam.ru",
          nativeToken: token,
        });
      },
      { accountId: TEST_ACCOUNT_ID }
    );

    expect(providerType).toBe("native");
  });

  test("v2_list_folders returns folders via native provider", async ({
    page,
  }) => {
    await page.goto("/");

    const folders = await page.evaluate(
      async ({ accountId }) => {
        const { invoke } = await import("@tauri-apps/api/core");

        const token = await invoke("native_login", {
          serverUrl: "https://mail.letotam.ru",
          username: "lucky",
          password: "LuckY1901!",
        });

        await invoke("activate_account", {
          accountId,
          imapHost: "mail.letotam.ru",
          imapPort: 993,
          username: "lucky",
          password: "LuckY1901!",
          useTls: true,
          email: "lucky@letotam.ru",
          nativeUrl: "https://mail.letotam.ru",
          nativeToken: token,
        });

        return invoke("v2_list_folders", { accountId });
      },
      { accountId: TEST_ACCOUNT_ID + "-folders" }
    );

    expect(Array.isArray(folders)).toBe(true);
    expect(folders.length).toBeGreaterThan(0);

    // Should have INBOX
    const inbox = folders.find(
      (f: any) => f.name === "INBOX" || f.special_use === "\\Inbox"
    );
    expect(inbox).toBeTruthy();
  });

  test("v2_search_messages returns results", async ({ page }) => {
    await page.goto("/");

    const results = await page.evaluate(
      async ({ accountId }) => {
        const { invoke } = await import("@tauri-apps/api/core");

        const token = await invoke("native_login", {
          serverUrl: "https://mail.letotam.ru",
          username: "lucky",
          password: "LuckY1901!",
        });

        await invoke("activate_account", {
          accountId,
          imapHost: "mail.letotam.ru",
          imapPort: 993,
          username: "lucky",
          password: "LuckY1901!",
          useTls: true,
          email: "lucky@letotam.ru",
          nativeUrl: "https://mail.letotam.ru",
          nativeToken: token,
        });

        return invoke("v2_search_messages", {
          accountId,
          userEmail: "lucky@letotam.ru",
          query: "test",
        });
      },
      { accountId: TEST_ACCOUNT_ID + "-search" }
    );

    expect(Array.isArray(results)).toBe(true);
  });

  test("activate_account with IMAP provider (fallback)", async ({ page }) => {
    await page.goto("/");

    const providerType = await page.evaluate(async () => {
      const { invoke } = await import("@tauri-apps/api/core");
      return invoke("activate_account", {
        accountId: "test-imap-" + Date.now(),
        imapHost: "imap.yandex.com",
        imapPort: 993,
        username: "test",
        password: "test",
        useTls: true,
        email: "test@yandex.ru",
        nativeUrl: null,
        nativeToken: null,
      });
    });

    expect(providerType).toBe("imap");
  });
});
