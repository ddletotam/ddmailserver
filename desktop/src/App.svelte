<script lang="ts">
  import Sidebar from "./lib/components/Sidebar.svelte";
  import ChatView from "./lib/components/ChatView.svelte";
  import LoginScreen from "./lib/components/LoginScreen.svelte";
  import { accountStore } from "./lib/stores/accounts.svelte";
  import { mailStore } from "./lib/stores/mail.svelte";
  import { identityStore } from "./lib/stores/identity.svelte";

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

  function handleAccountAdded() {
    showLogin = false;
  }
</script>

{#if showLogin}
  <LoginScreen onSuccess={handleAccountAdded} />
{:else}
  <div class="app-layout" style:--sidebar-width="{sidebarWidth}px">
    <Sidebar />
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div class="splitter" onmousedown={startDragSplitter} role="separator" aria-orientation="vertical" tabindex="-1"></div>
    <ChatView />
  </div>
{/if}

<style>
  .app-layout {
    display: flex;
    height: 100vh;
    width: 100vw;
    overflow: hidden;
  }
  /* 4-px wide drag handle. Sits in the flex flow between sidebar and chat
     view; cursor changes on hover so the user discovers it. Hairline visual
     uses border-color so it blends with the sidebar's existing right border
     in non-dragged state. */
  .splitter {
    width: 4px;
    flex-shrink: 0;
    cursor: col-resize;
    background: transparent;
    transition: background 0.1s;
  }
  .splitter:hover,
  .splitter:active {
    background: var(--text-accent);
  }
</style>
