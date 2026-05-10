<script lang="ts">
  import { themeStore } from "../stores/theme.svelte";
  import { WebviewWindow } from "@tauri-apps/api/webviewWindow";

  let open = $state(false);

  function toggle(e: MouseEvent) {
    e.stopPropagation();
    open = !open;
  }

  function close() {
    open = false;
  }

  function handleTheme() {
    themeStore.toggle();
    open = false;
  }

  async function handleCalendar() {
    open = false;
    try {
      const existing = await WebviewWindow.getByLabel("calendar");
      if (existing) {
        await existing.setFocus();
        return;
      }
      const win = new WebviewWindow("calendar", {
        url: "index.html?view=calendar",
        title: "DDMail — Календарь",
        width: 1100,
        height: 700,
        minWidth: 700,
        minHeight: 500,
      });
      win.once("tauri://error", (e) => console.error("[calendar window]", e));
    } catch (e) {
      console.error("[hamburger] open calendar failed:", e);
    }
  }
</script>

<svelte:window onclick={close} />

<div class="wrap">
  <button class="btn-icon" onclick={toggle} title="Меню" aria-haspopup="menu" aria-expanded={open}>
    <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
      <line x1="4" y1="6" x2="20" y2="6"/>
      <line x1="4" y1="12" x2="20" y2="12"/>
      <line x1="4" y1="18" x2="20" y2="18"/>
    </svg>
  </button>

  {#if open}
    <!-- Stop propagation so clicks on items don't immediately re-close via window listener -->
    <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
    <div class="menu" role="menu" onclick={(e) => e.stopPropagation()}>
      <button class="item" role="menuitem" onclick={handleTheme}>
        {#if themeStore.isDark}
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
            <circle cx="12" cy="12" r="5"/><line x1="12" y1="1" x2="12" y2="3"/><line x1="12" y1="21" x2="12" y2="23"/>
            <line x1="4.22" y1="4.22" x2="5.64" y2="5.64"/><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"/>
            <line x1="1" y1="12" x2="3" y2="12"/><line x1="21" y1="12" x2="23" y2="12"/>
            <line x1="4.22" y1="19.78" x2="5.64" y2="18.36"/><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"/>
          </svg>
          <span>Светлая тема</span>
        {:else}
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
            <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/>
          </svg>
          <span>Тёмная тема</span>
        {/if}
      </button>

      <button class="item" role="menuitem" onclick={handleCalendar}>
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <rect x="3" y="4" width="18" height="18" rx="2" ry="2"/>
          <line x1="16" y1="2" x2="16" y2="6"/>
          <line x1="8" y1="2" x2="8" y2="6"/>
          <line x1="3" y1="10" x2="21" y2="10"/>
        </svg>
        <span>Календарь</span>
      </button>
    </div>
  {/if}
</div>

<style>
  .wrap { position: relative; flex-shrink: 0; }

  .btn-icon {
    width: 40px; height: 40px;
    display: flex; align-items: center; justify-content: center;
    border: none; background: none; border-radius: 50%;
    cursor: pointer; color: var(--text-secondary);
    transition: background var(--transition);
  }
  .btn-icon:hover { background: var(--bg-hover); }

  .menu {
    position: absolute;
    top: calc(100% + 4px);
    left: 0;
    min-width: 200px;
    background: var(--bg-primary);
    border: 1px solid var(--border-color);
    border-radius: 10px;
    box-shadow: var(--shadow-md);
    padding: 4px;
    z-index: 300;
  }

  .item {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    padding: 8px 12px;
    border: none;
    background: none;
    color: var(--text-primary);
    font-family: var(--font-family);
    font-size: var(--font-size);
    text-align: left;
    border-radius: 6px;
    cursor: pointer;
  }
  .item:hover { background: var(--bg-hover); }
  .item :global(svg) { color: var(--text-secondary); flex-shrink: 0; }
</style>
