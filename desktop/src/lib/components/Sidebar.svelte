<script lang="ts">
  import { accountStore } from "../stores/accounts.svelte";
  import { mailStore } from "../stores/mail.svelte";
  import { identityStore } from "../stores/identity.svelte";
  import ConversationItem from "./ConversationItem.svelte";
  import SearchDropdown from "./SearchDropdown.svelte";
  import HamburgerMenu from "./HamburgerMenu.svelte";
  import type { MessageEnvelope, Contact } from "../types/mail";

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

  function firstIdentityEmail(): string {
    return (identityStore.defaultIdentity?.email ?? identityStore.identities[0]?.email
      ?? accountStore.activeAccount?.email ?? "").toLowerCase();
  }

  function resetSearch() {
    searchQuery = "";
    showSearch = false;
    mailStore.clearSearch();
  }

  function handleComposeNew(email: string) {
    mailStore.setComposeIntent({ to: email, focusField: "subject" });
    resetSearch();
  }

  function handleSelectContact(c: Contact) {
    const account = accountStore.activeAccount;
    if (!account) return;
    const cpLc = c.email.toLowerCase();
    // Find ANY conversation with this counterpart, regardless of which identity. If there
    // are several (same person via different identities), prefer the most recent one.
    const candidates = mailStore.conversations.filter(
      (cv) => (cv.counterparts[0]?.addr ?? "").toLowerCase() === cpLc,
    );
    if (candidates.length > 0) {
      const best = candidates.reduce((a, b) => (a.last_date_ts >= b.last_date_ts ? a : b));
      mailStore.openConversation(account, best.id);
    } else {
      // No existing conversation — open Composer pre-filled to that contact.
      mailStore.setComposeIntent({ to: c.email, focusField: "subject" });
    }
    resetSearch();
  }

  function handleSelectMessage(msg: MessageEnvelope) {
    const account = accountStore.activeAccount;
    if (!account) return;

    const ourAddrs = new Set(
      identityStore.identities.map((i) => i.email.toLowerCase())
        .concat([account.email.toLowerCase()]),
    );
    const fromLc = msg.from_addr.toLowerCase();
    const isOutgoing = ourAddrs.has(fromLc);
    const cpLc = isOutgoing
      ? (msg.to_addrs.find((a) => !ourAddrs.has(a.toLowerCase())) ?? msg.to_addrs[0] ?? "").toLowerCase()
      : fromLc;

    // The conversation that *actually* owns this message is the only one whose
    // messages list contains the (folder, uid) pair. Don't fall back to "any
    // conversation with this counterpart" — that triggers a phantom jumpIntent
    // against a thread that doesn't have the message and the highlight never
    // fires. If it's nowhere local, the message exists on the server but we
    // haven't pulled it into a conversation yet (or it was deleted).
    const conv = mailStore.conversations.find((c) =>
      (c.counterparts[0]?.addr ?? "").toLowerCase() === cpLc &&
      c.messages.some((mr) => mr.folder === msg.folder && mr.uid === msg.uid),
    );
    if (!conv) {
      alert(
        "Письмо есть на сервере, но в локальной выборке диалога его нет.\n" +
        "Возможно, оно удалено — проверьте корзину в веб-интерфейсе сервера, " +
        "либо перезагрузите список диалогов (Ctrl+R), чтобы подтянуть свежее."
      );
      resetSearch();
      return;
    }
    mailStore.setJumpIntent({ folder: msg.folder, uid: msg.uid });
    mailStore.openConversation(account, conv.id);
    resetSearch();
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

  async function handleDelete() {
    if (!contextMenu) return;
    const id = contextMenu.convId;
    contextMenu = null;
    const account = accountStore.activeAccount;
    const conv = mailStore.conversations.find((c) => c.id === id);
    if (!account || !conv) return;
    try {
      await mailStore.deleteConversation(account, conv);
    } catch (e) {
      alert(`Не удалось удалить: ${e}`);
    }
  }

  async function handleMarkRead() {
    if (!contextMenu) return;
    const id = contextMenu.convId;
    contextMenu = null;
    const account = accountStore.activeAccount;
    const conv = mailStore.conversations.find((c) => c.id === id);
    if (!account || !conv) return;
    await mailStore.markConversationRead(account, conv);
  }

  async function handleMarkAllRead() {
    contextMenu = null;
    const account = accountStore.activeAccount;
    if (!account) return;
    await mailStore.markAllRead(account);
  }

  async function handleSpam() {
    if (!contextMenu) return;
    const id = contextMenu.convId;
    contextMenu = null;
    const account = accountStore.activeAccount;
    const conv = mailStore.conversations.find((c) => c.id === id);
    if (!account || !conv) return;
    const addr = (conv.counterparts[0]?.addr ?? "").toLowerCase();
    const at = addr.lastIndexOf("@");
    if (at <= 0) {
      alert("У отправителя нет адреса с доменом — нечего блокировать.");
      return;
    }
    const domain = addr.slice(at + 1);
    if (!confirm(`Пометить весь этот диалог как спам и заблокировать домен @${domain}?`)) return;
    try {
      await mailStore.markConversationSpam(account, conv);
    } catch (e) {
      alert(`Не удалось пометить как спам: ${e}`);
    }
  }

  // Keyboard navigation for the conversation list.
  // - ArrowUp / ArrowDown: move selection (also opens the conv so the right
  //   pane stays in sync — same UX as the Telegram desktop client).
  // - Enter: focus current active conv; useful when nothing is selected yet.
  // - Delete: delete the active conv after confirm.
  //
  // We bail out the moment the focused element is editable so users can keep
  // typing in the search box, the composer body, advanced fields, etc.
  function isEditableTarget(t: EventTarget | null): boolean {
    if (!(t instanceof HTMLElement)) return false;
    const tag = t.tagName;
    if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return true;
    if (t.isContentEditable) return true;
    return false;
  }

  async function handleListShortcut(e: KeyboardEvent) {
    if (isEditableTarget(e.target)) return;
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    const list = mailStore.conversations;
    if (list.length === 0) return;
    const activeId = mailStore.activeConversationId;
    const idx = activeId ? list.findIndex(c => c.id === activeId) : -1;
    const account = accountStore.activeAccount;
    if (!account) return;

    if (e.key === "ArrowDown") {
      e.preventDefault();
      const next = idx < 0 ? 0 : Math.min(list.length - 1, idx + 1);
      if (next !== idx) {
        await mailStore.openConversation(account, list[next].id);
        scrollActiveIntoView();
      }
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      const next = idx <= 0 ? 0 : idx - 1;
      if (next !== idx) {
        await mailStore.openConversation(account, list[next].id);
        scrollActiveIntoView();
      }
    } else if (e.key === "Enter") {
      if (idx < 0) {
        e.preventDefault();
        await mailStore.openConversation(account, list[0].id);
        scrollActiveIntoView();
      }
    } else if (e.key === "Delete") {
      if (idx < 0) return;
      const conv = list[idx];
      if (!confirm(`Удалить диалог «${conv.label}»?`)) return;
      e.preventDefault();
      try {
        await mailStore.deleteConversation(account, conv);
      } catch (err) {
        alert(`Не удалось удалить: ${err}`);
      }
    }
  }

  // After arrow-nav we want the new active row visible. Pure CSS focus
  // wouldn't help here — the items are buttons but we don't actually shift
  // DOM focus, the highlight comes from `active` class.
  function scrollActiveIntoView() {
    requestAnimationFrame(() => {
      const id = mailStore.activeConversationId;
      if (!id) return;
      document.querySelector('.conv-item.active')?.scrollIntoView({ block: "nearest" });
    });
  }
</script>

<svelte:window onclick={closeContextMenu} onkeydown={handleListShortcut} />

<aside class="sidebar">
  <!-- Search -->
  <div class="sidebar-header">
    <HamburgerMenu />
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
          query={searchQuery}
          contacts={mailStore.searchContacts}
          results={mailStore.searchResults}
          loading={mailStore.searchLoading}
          oncomposeNew={handleComposeNew}
          onselectContact={handleSelectContact}
          onselectMessage={handleSelectMessage}
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
    {@const ctxConv = mailStore.conversations.find((c) => c.id === contextMenu!.convId)}
    <div class="context-menu" style:left="{contextMenu.x}px" style:top="{contextMenu.y}px">
      <button onclick={handlePin}>
        {mailStore.isPinned(contextMenu.convId) ? "Открепить" : "Закрепить наверху"}
      </button>
      <button onclick={handleDelete}>Удалить</button>
      <button onclick={handleMarkRead} disabled={!ctxConv || ctxConv.unread_count === 0}>
        Пометить прочитанным
      </button>
      <button onclick={handleSpam} class="ctx-danger">В спам по домену</button>
      <div class="ctx-separator"></div>
      <button onclick={handleMarkAllRead}>Все прочитанные</button>
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

  .context-menu button:hover:not(:disabled) {
    background: var(--bg-hover);
  }
  .context-menu button.ctx-danger { color: #c0392b; }
  .context-menu button.ctx-danger:hover:not(:disabled) { background: rgba(192, 57, 43, 0.08); }
  .context-menu button:disabled {
    color: var(--text-secondary);
    cursor: default;
    opacity: 0.55;
  }
  .ctx-separator {
    height: 1px;
    background: var(--border-color);
    margin: 4px 0;
  }
</style>
