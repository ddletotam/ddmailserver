<script lang="ts">
  import { accountStore } from "../stores/accounts.svelte";
  import { mailStore } from "../stores/mail.svelte";
  import { themeStore } from "../stores/theme.svelte";
  import ConversationItem from "./ConversationItem.svelte";
  import SearchDropdown from "./SearchDropdown.svelte";
  import type { MessageEnvelope } from "../types/mail";

  // Track pinned/unpinned boundary for divider
  const hasPinned = $derived(mailStore.conversations.some(c => mailStore.isPinned(c.id)));
  function isPinnedBoundary(i: number): boolean {
    if (!hasPinned) return false;
    const convs = mailStore.conversations;
    if (i === 0) return false;
    const prevPinned = mailStore.isPinned(convs[i - 1].id);
    const curPinned = mailStore.isPinned(convs[i].id);
    return prevPinned && !curPinned;
  }

  let searchQuery = $state("");
  let searchTimeout: ReturnType<typeof setTimeout>;
  let showSearch = $state(false);

  // Context menu
  let contextMenu = $state<{ x: number; y: number; convId: string } | null>(null);

  function handleSearchInput(value: string) {
    clearTimeout(searchTimeout);
    if (!value.trim()) {
      showSearch = false;
      mailStore.clearSearch();
      return;
    }
    showSearch = true;
    searchTimeout = setTimeout(() => {
      const account = accountStore.activeAccount;
      if (account) {
        mailStore.search(account, value);
      }
    }, 400);
  }

  function handleSearchSelect(msg: MessageEnvelope) {
    // Find the conversation containing this message, or open it directly
    const account = accountStore.activeAccount;
    if (!account) return;

    // Try to find conversation by from_addr
    const addr = msg.is_outgoing
      ? msg.to_addrs[0] ?? ""
      : msg.from_addr;
    const conv = mailStore.conversations.find(
      (c) => c.id === addr || c.counterparts.some((cp) => cp.addr === addr)
    );
    if (conv) {
      mailStore.openConversation(account, conv.id);
    }
    searchQuery = "";
    showSearch = false;
    mailStore.clearSearch();
  }

  function handleSearchBlur() {
    // Delay to allow click on dropdown items
    setTimeout(() => {
      showSearch = false;
    }, 200);
  }

  function handleContextMenu(e: MouseEvent, convId: string) {
    e.preventDefault();
    contextMenu = { x: e.clientX, y: e.clientY, convId };
  }

  function closeContextMenu() {
    contextMenu = null;
  }

  function handlePin() {
    if (contextMenu) {
      mailStore.togglePin(contextMenu.convId);
      contextMenu = null;
    }
  }
</script>

<svelte:window onclick={closeContextMenu} />

<aside class="sidebar">
  <!-- Search -->
  <div class="sidebar-header">
    <!-- Theme toggle -->
    <button class="btn-icon" onclick={() => themeStore.toggle()} title={themeStore.isDark ? "Light mode" : "Dark mode"}>
      {#if themeStore.isDark}
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <circle cx="12" cy="12" r="5"/><line x1="12" y1="1" x2="12" y2="3"/><line x1="12" y1="21" x2="12" y2="23"/>
          <line x1="4.22" y1="4.22" x2="5.64" y2="5.64"/><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"/>
          <line x1="1" y1="12" x2="3" y2="12"/><line x1="21" y1="12" x2="23" y2="12"/>
          <line x1="4.22" y1="19.78" x2="5.64" y2="18.36"/><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"/>
        </svg>
      {:else}
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/>
        </svg>
      {/if}
    </button>
    <div class="search-box" class:active={showSearch}>
      <svg class="search-icon" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
        <circle cx="11" cy="11" r="8" /><line x1="21" y1="21" x2="16.65" y2="16.65" />
      </svg>
      <input
        type="text"
        placeholder="Search"
        bind:value={searchQuery}
        oninput={(e) => handleSearchInput(e.currentTarget.value)}
        onfocus={() => { if (searchQuery.trim()) showSearch = true; }}
        onblur={handleSearchBlur}
      />
      {#if searchQuery}
        <button class="btn-clear" onclick={() => {
          searchQuery = "";
          showSearch = false;
          mailStore.clearSearch();
        }} title="Clear">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
            <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
          </svg>
        </button>
      {/if}

      {#if showSearch}
        <SearchDropdown
          results={mailStore.searchResults}
          loading={mailStore.searchLoading}
          onselect={handleSearchSelect}
        />
      {/if}
    </div>
  </div>

  <!-- Conversations list -->
  <div class="conversation-list">
    {#if mailStore.loading && mailStore.conversations.length === 0}
      <div class="status">Loading...</div>
    {:else if mailStore.conversations.length === 0}
      <div class="status">No conversations</div>
    {:else}
      {#each mailStore.conversations as conv, i (conv.id)}
        {#if isPinnedBoundary(i)}
          <div class="pinned-divider"></div>
        {/if}
        <ConversationItem
          conversation={conv}
          active={mailStore.activeConversationId === conv.id}
          pinned={mailStore.isPinned(conv.id)}
          onclick={() => {
            const account = accountStore.activeAccount;
            if (account) mailStore.openConversation(account, conv.id);
          }}
          oncontextmenu={(e) => handleContextMenu(e, conv.id)}
        />
      {/each}
    {/if}
  </div>

  <!-- Context menu -->
  {#if contextMenu}
    <div class="context-menu" style:left="{contextMenu.x}px" style:top="{contextMenu.y}px">
      <button onclick={handlePin}>
        {mailStore.isPinned(contextMenu.convId) ? "Unpin" : "Pin to top"}
      </button>
    </div>
  {/if}
</aside>

<style>
  .sidebar {
    width: var(--sidebar-width);
    min-width: var(--sidebar-width);
    max-width: var(--sidebar-width);
    height: 100vh;
    display: flex;
    flex-direction: column;
    background: var(--bg-sidebar);
    border-right: 1px solid var(--border-color);
    position: relative;
  }

  .sidebar-header {
    padding: 8px;
    border-bottom: 1px solid var(--border-color);
    height: var(--header-height);
    display: flex;
    align-items: center;
    gap: 4px;
  }

  .btn-icon {
    width: 40px; height: 40px;
    display: flex; align-items: center; justify-content: center;
    border: none; background: none; border-radius: 50%;
    cursor: pointer; color: var(--text-secondary); flex-shrink: 0;
    transition: background var(--transition);
  }
  .btn-icon:hover { background: var(--bg-hover); }

  .search-box {
    flex: 1;
    position: relative;
  }

  .search-icon {
    position: absolute;
    left: 10px;
    top: 50%;
    transform: translateY(-50%);
    color: var(--text-secondary);
    pointer-events: none;
  }

  .search-box input {
    width: 100%;
    padding: 8px 36px 8px 36px;
    background: var(--bg-secondary);
    border: 2px solid transparent;
    border-radius: 20px;
    font-size: var(--font-size);
    font-family: var(--font-family);
    outline: none;
    color: var(--text-primary);
    transition: border-color var(--transition), background var(--transition);
  }

  .search-box input::placeholder {
    color: var(--text-secondary);
  }

  .search-box.active input {
    border-color: var(--text-accent);
    background: var(--bg-primary);
    border-radius: 20px 20px 0 0;
  }

  .btn-clear {
    position: absolute;
    right: 6px;
    top: 50%;
    transform: translateY(-50%);
    width: 24px;
    height: 24px;
    display: flex;
    align-items: center;
    justify-content: center;
    border: none;
    background: none;
    border-radius: 50%;
    cursor: pointer;
    color: var(--text-secondary);
  }

  .btn-clear:hover {
    background: var(--bg-hover);
  }

  .conversation-list {
    flex: 1;
    overflow-y: auto;
  }

  .pinned-divider {
    height: 1px;
    background: var(--border-color);
    margin: 4px 12px;
  }

  .status {
    display: flex;
    align-items: center;
    justify-content: center;
    height: 100px;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .context-menu {
    position: fixed;
    background: var(--bg-primary);
    border: 1px solid var(--border-color);
    border-radius: 8px;
    box-shadow: var(--shadow-md);
    z-index: 200;
    overflow: hidden;
  }

  .context-menu button {
    display: block;
    width: 100%;
    padding: 8px 16px;
    border: none;
    background: none;
    cursor: pointer;
    font-size: var(--font-size-sm);
    font-family: var(--font-family);
    text-align: left;
    white-space: nowrap;
    color: var(--text-primary);
  }

  .context-menu button:hover {
    background: var(--bg-hover);
  }
</style>
