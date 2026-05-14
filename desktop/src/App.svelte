<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import { invoke } from "@tauri-apps/api/core";
  import { listen, type UnlistenFn } from "@tauri-apps/api/event";
  import { WebviewWindow } from "@tauri-apps/api/webviewWindow";
  import Sidebar from "./lib/components/Sidebar.svelte";
  import ChatView from "./lib/components/ChatView.svelte";
  import LoginScreen from "./lib/components/LoginScreen.svelte";
  import CalendarView from "./lib/components/CalendarView.svelte";
  import { accountStore } from "./lib/stores/accounts.svelte";
  import { mailStore } from "./lib/stores/mail.svelte";
  import { identityStore } from "./lib/stores/identity.svelte";

  // Secondary windows (Calendar, future Settings) are loaded as the same
  // SPA bundle with `?view=…` — keeps webview management trivial without
  // pulling in a router. The mail UI is the default.
  const view = new URLSearchParams(window.location.search).get("view");

  let showLogin = $state(accountStore.accounts.length === 0);

  // Sidebar width — user-resizable via splitter, persisted in localStorage.
  // Bounds chosen so neither pane disappears: minimum still readable, maximum
  // leaves at least 400px for the conversation view on a 1024px window.
  const SIDEBAR_KEY = "ddmail_sidebar_width";
  const SIDEBAR_MIN = 240;
  const SIDEBAR_MAX = 700;
  function loadSidebarWidth(): number {
    const raw = localStorage.getItem(SIDEBAR_KEY);
    const n = raw ? parseInt(raw, 10) : NaN;
    return Number.isFinite(n) && n >= SIDEBAR_MIN && n <= SIDEBAR_MAX ? n : 380;
  }
  let sidebarWidth = $state(loadSidebarWidth());

  function startDragSplitter(e: MouseEvent) {
    e.preventDefault();
    const startX = e.clientX;
    const startW = sidebarWidth;
    const onMove = (ev: MouseEvent) => {
      const dx = ev.clientX - startX;
      sidebarWidth = Math.min(SIDEBAR_MAX, Math.max(SIDEBAR_MIN, startW + dx));
    };
    const onUp = () => {
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
      try { localStorage.setItem(SIDEBAR_KEY, String(sidebarWidth)); } catch {}
    };
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
  }

  $effect(() => {
    if (view === "calendar") return; // calendar window doesn't load mail
    const account = accountStore.activeAccount;
    if (!account) return;
    // Identities must be available BEFORE the conversation grouping runs — the
    // server-side `fetch_conversations` derives "our addresses" from the cached
    // identity list to compute (counterpart, my_identity) pairs. If we don't wait,
    // a fresh login groups everything under the single account.email and aliases
    // appear as self-threads.
    (async () => {
      try {
        await mailStore.ensureActivated(account);
        await identityStore.load(account);
        await mailStore.loadConversations(account);
      } catch (e) {
        console.error("[app] startup failed:", e);
      }
    })();
  });

  // Push total-unread count to the OS tray whenever it changes so the
  // Linux backend can composite the notification dot. Only the main
  // window owns the tray — the calendar window has no conversations to
  // count.
  $effect(() => {
    if (view === "calendar") return;
    const total = mailStore.conversations.reduce(
      (sum, c) => sum + (c.unread_count > 0 ? 1 : 0),
      0,
    );
    invoke("set_tray_unread", { count: total }).catch(() => {});
  });

  // Reminder-toast routing — only the MAIN window needs to handle
  // "open-event" from this side: when the calendar window is closed and
  // the user clicks a reminder, we have to open the calendar with the
  // event id in the URL so the freshly-mounted CalendarView knows what
  // to show. If the calendar window is already alive it handles the
  // event itself; we only set focus and bail.
  let unlistenOpenEvent: UnlistenFn | null = null;
  onMount(async () => {
    if (view === "calendar") return; // calendar handles its own
    try {
      unlistenOpenEvent = await listen<{ event_id: number; occurrence_start_ms: number }>(
        "open-event",
        async (e) => {
          const { event_id, occurrence_start_ms } = e.payload;
          const existing = await WebviewWindow.getByLabel("calendar");
          if (existing) {
            await existing.setFocus().catch(() => {});
            await existing.unminimize().catch(() => {});
            // calendar window's own listener will open the modal
            return;
          }
          const win = new WebviewWindow("calendar", {
            url: `index.html?view=calendar&open=${event_id}:${occurrence_start_ms}`,
            title: "DDMail — Календарь",
            width: 1100,
            height: 700,
            minWidth: 700,
            minHeight: 500,
          });
          win.once("tauri://error", (err) => console.error("[reminders] open calendar:", err));
        },
      );
    } catch (e) {
      console.warn("[reminders] open-event listener failed:", e);
    }
  });
  onDestroy(() => {
    if (unlistenOpenEvent) unlistenOpenEvent();
  });

  function handleAccountAdded() {
    showLogin = false;
  }
</script>

{#if view === "calendar"}
  <CalendarView />
{:else if showLogin}
  <LoginScreen onSuccess={handleAccountAdded} />
{:else}
  <div class="app-layout" style="--sidebar-width: {sidebarWidth}px">
    <Sidebar />
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div class="splitter" onmousedown={startDragSplitter} role="separator" aria-orientation="vertical" tabindex="-1"></div>
    <ChatView />
  </div>
{/if}

<!-- App layout styles live in app.css (global) because Svelte 5's scoped CSS
     pass intermittently strips `.app-layout` from this file, leaving the flex
     layout collapsed and the chat pane invisible. -->
