<script lang="ts">
  import { invoke } from "@tauri-apps/api/core";
  import { accountStore } from "../stores/accounts.svelte";
  import { mailStore } from "../stores/mail.svelte";
  import { identityStore } from "../stores/identity.svelte";
  import MessageBubble from "./MessageBubble.svelte";
  import Composer from "./Composer.svelte";
  import { cleanName, sameDay, formatDateSeparator } from "../utils/format";
  import type { OutgoingMessage } from "../types/mail";

  let showComposer = $state(false);
  let replyMode = $state<"reply" | "forward" | null>(null);
  let chatContainer = $state<HTMLDivElement | null>(null);
  let showScrollBtn = $state(false);

  const conv = $derived(mailStore.activeConversation);
  const msgs = $derived(mailStore.conversationMessages);
  const lastMessage = $derived(msgs.length > 0 ? msgs[msgs.length - 1] : null);

  // Message grouping: compute first/last in sender group
  function isFirstInGroup(i: number): boolean {
    if (i === 0) return true;
    return msgs[i].from_addr !== msgs[i - 1].from_addr;
  }
  function isLastInGroup(i: number): boolean {
    if (i === msgs.length - 1) return true;
    return msgs[i].from_addr !== msgs[i + 1].from_addr;
  }

  // Auto-scroll to bottom
  $effect(() => {
    if (msgs.length > 0 && chatContainer) {
      requestAnimationFrame(() => {
        if (chatContainer) {
          chatContainer.scrollTop = chatContainer.scrollHeight;
          showScrollBtn = false;
        }
      });
    }
  });

  function handleScroll() {
    if (!chatContainer) return;
    const { scrollTop, scrollHeight, clientHeight } = chatContainer;
    showScrollBtn = scrollHeight - scrollTop - clientHeight > 200;
  }

  function scrollToBottom() {
    if (chatContainer) {
      chatContainer.scrollTo({ top: chatContainer.scrollHeight, behavior: "smooth" });
    }
  }

  function handleReply() { replyMode = "reply"; showComposer = true; }
  function handleForward() { replyMode = "forward"; showComposer = true; }
  function handleCompose() { replyMode = null; showComposer = true; }
  function closeComposer() { showComposer = false; replyMode = null; }

  // Quick-reply
  let quickReplySending = $state(false);
  let identityDropdownOpen = $state(false);
  let replyEditorRef = $state<HTMLDivElement | null>(null);
  let formatMenu = $state<{ x: number; y: number } | null>(null);

  // Load draft into editor when conversation has a draft
  $effect(() => {
    const draft = mailStore.draftMessage;
    if (draft && conv && replyEditorRef) {
      replyEditorRef.innerText = draft.text || "";
    }
  });

  function getEditorText(): string {
    return replyEditorRef?.innerText?.trim() ?? "";
  }

  function getEditorHtml(): string {
    if (!replyEditorRef) return "";
    let html = replyEditorRef.innerHTML;
    // Clean up contenteditable artifacts
    html = html.replace(/<div><br><\/div>/gi, "<br>");
    html = html.replace(/<div>/gi, "<br>");
    html = html.replace(/<\/div>/gi, "");
    if (html === "<br>") return "";
    return html;
  }

  function clearEditor() {
    if (replyEditorRef) replyEditorRef.innerHTML = "";
  }

  function handleEditorKeydown(e: KeyboardEvent) {
    // Ctrl+B/I/U
    if (e.ctrlKey || e.metaKey) {
      if (e.key === "b") { e.preventDefault(); document.execCommand("bold"); }
      else if (e.key === "i") { e.preventDefault(); document.execCommand("italic"); }
      else if (e.key === "u") { e.preventDefault(); document.execCommand("underline"); }
    }
    // Enter to send, Shift+Enter for newline
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      sendQuickReply();
    }
  }

  function handleEditorPaste(e: ClipboardEvent) {
    // Auto-linkify pasted URLs
    const text = e.clipboardData?.getData("text/plain");
    if (text && /^https?:\/\/\S+$/.test(text.trim())) {
      e.preventDefault();
      const url = text.trim();
      document.execCommand("insertHTML", false, `<a href="${url}">${url}</a>`);
    }
  }

  function handleEditorInput() {
    // Auto-linkify disabled — was causing input event loops
  }

  function showFormatMenu(e: MouseEvent) {
    e.preventDefault();
    const menuW = 160, menuH = 150;
    const x = Math.min(e.clientX, window.innerWidth - menuW);
    const y = Math.min(e.clientY, window.innerHeight - menuH);
    formatMenu = { x, y };
  }

  function closeFormatMenu() {
    formatMenu = null;
  }

  function execFormat(cmd: string) {
    document.execCommand(cmd);
    formatMenu = null;
    replyEditorRef?.focus();
  }

  function insertLink() {
    formatMenu = null;
    const url = prompt("URL:");
    if (!url) return;
    const sel = window.getSelection();
    const text = sel && sel.toString().trim() ? sel.toString() : url;
    document.execCommand("insertHTML", false, `<a href="${url}">${text}</a>`);
    replyEditorRef?.focus();
  }

  // Identity for this conversation — pre-select based on received_by
  const matchedIdentity = $derived(
    conv ? identityStore.findByEmail(conv.received_by) : null
  );
  let selectedFromEmail = $state("");
  $effect(() => {
    if (matchedIdentity) {
      selectedFromEmail = matchedIdentity.email;
    } else if (identityStore.defaultIdentity) {
      selectedFromEmail = identityStore.defaultIdentity.email;
    }
  });

  async function sendQuickReply() {
    const account = accountStore.activeAccount;
    const text = getEditorText();
    if (!account || !text || !conv || !lastMessage) return;

    quickReplySending = true;
    try {
      const replyTo = lastMessage.from_addr;
      const subject = lastMessage.subject.startsWith("Re:")
        ? lastMessage.subject
        : `Re: ${lastMessage.subject}`;
      const html = getEditorHtml();
      const msg: OutgoingMessage = {
        from: selectedFromEmail || account.email,
        to: [replyTo],
        cc: [],
        subject,
        text,
        html: html ? `<div style="font-family:sans-serif;font-size:14px">${html}</div>` : `<div style="font-family:sans-serif;font-size:14px">${text.replace(/\n/g, "<br>")}</div>`,
        in_reply_to: null,
        references: null,
      };
      await invoke("send_message", {
        ...mailStore.smtpArgs(account),
        message: msg,
      });
      clearEditor();
      // Refresh conversation
      mailStore.openConversation(account, conv.id);
    } catch (e) {
      console.error("Send failed:", e);
    } finally {
      quickReplySending = false;
    }
  }


  // Keyboard shortcuts
  function handleKeydown(e: KeyboardEvent) {
    if (e.key === "Escape") {
      if (identityDropdownOpen) { identityDropdownOpen = false; e.preventDefault(); }
      else if (showComposer) { closeComposer(); e.preventDefault(); }
      else if (conv) { mailStore.closeConversation(); e.preventDefault(); }
    }
  }

  function handleGlobalClick(e: MouseEvent) {
    if (identityDropdownOpen) {
      const target = e.target as HTMLElement;
      if (!target.closest('.identity-picker')) {
        identityDropdownOpen = false;
      }
    }
    if (formatMenu) {
      const target = e.target as HTMLElement;
      if (!target.closest('.format-menu')) {
        formatMenu = null;
      }
    }
  }
</script>

<svelte:window onkeydown={handleKeydown} onclick={handleGlobalClick} />

<main class="chat-view">
  {#if conv}
    <!-- Header -->
    <div class="chat-header">
      <button class="btn-back" onclick={() => mailStore.closeConversation()} title="Back">
        <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <polyline points="15 18 9 12 15 6" />
        </svg>
      </button>

      <div class="chat-info">
        <div class="chat-name">{cleanName(conv.label)}</div>
        <div class="chat-meta">
          {#if conv.is_group}
            {conv.counterparts.map(cp => cp.name || cp.addr).join(", ")}
          {:else}
            {conv.counterparts[0]?.addr ?? ""}
          {/if}
          {#if mailStore.connectionState === "connected"}
            <span class="conn-dot connected" title="Connected"></span>
          {:else if mailStore.connectionState === "connecting"}
            <span class="conn-dot connecting" title="Connecting..."></span>
          {/if}
        </div>
      </div>

      <div class="chat-actions">
        <button class="btn-icon" onclick={handleReply} title="Reply">
          <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
            <polyline points="9 17 4 12 9 7" /><path d="M20 18v-2a4 4 0 0 0-4-4H4" />
          </svg>
        </button>
        <button class="btn-icon" onclick={handleForward} title="Forward">
          <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
            <polyline points="15 17 20 12 15 7" /><path d="M4 18v-2a4 4 0 0 1 4-4h12" />
          </svg>
        </button>
        <button class="btn-icon" onclick={handleCompose} title="New message">
          <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
            <path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7" />
            <path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z" />
          </svg>
        </button>
      </div>
    </div>

    <!-- Messages -->
    <div class="chat-messages" bind:this={chatContainer} onscroll={handleScroll}>
      {#if mailStore.error}
        <div class="center-status">
          <div class="error-box">{mailStore.error}</div>
        </div>
      {:else if mailStore.loadingMessages}
        <div class="center-status">
          <div class="spinner"></div>
        </div>
      {:else if msgs.length === 0}
        <div class="center-status">No messages yet</div>
      {:else}
        {#each msgs as msg, i (msg.uid)}
          <MessageBubble
            message={msg}
            isFirstInGroup={isFirstInGroup(i)}
            isLastInGroup={isLastInGroup(i)}
          />
        {/each}
      {/if}
    </div>

    <!-- Scroll to bottom FAB -->
    {#if showScrollBtn}
      <button class="scroll-fab" onclick={scrollToBottom} title="Scroll to bottom">
        <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5">
          <polyline points="6 9 12 15 18 9" />
        </svg>
      </button>
    {/if}

    <!-- Quick-reply input (Telegram-style) -->
    {#if showComposer}
      <Composer mode={replyMode} originalMessage={lastMessage} onclose={closeComposer} />
    {:else}
      <div class="quick-reply">
        <div class="reply-input-row">
          <!-- Identity selector (if multiple) -->
          {#if identityStore.hasMultiple}
            <div class="identity-picker">
              <button
                class="identity-picker-btn"
                onclick={() => identityDropdownOpen = !identityDropdownOpen}
                title="Send from"
              >
                <span class="identity-dot" style:background={identityStore.findByEmail(selectedFromEmail)?.color ?? '#ccc'}></span>
                <span class="identity-email">{selectedFromEmail}</span>
                <svg class="identity-chevron" class:open={identityDropdownOpen} width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                  <polyline points="6 9 12 15 18 9" />
                </svg>
              </button>
              {#if identityDropdownOpen}
                <div class="identity-dropdown">
                  {#each identityStore.identities as id}
                    <button
                      class="identity-option"
                      class:selected={id.email === selectedFromEmail}
                      onclick={() => { selectedFromEmail = id.email; identityDropdownOpen = false; }}
                    >
                      <span class="identity-dot" style:background={id.color}></span>
                      <span class="identity-option-email">{id.email}</span>
                      {#if id.email === selectedFromEmail}
                        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5">
                          <polyline points="20 6 9 17 4 12" />
                        </svg>
                      {/if}
                    </button>
                  {/each}
                </div>
              {/if}
            </div>
          {/if}

          <!-- Rich text editor -->
          <!-- svelte-ignore a11y_no_static_element_interactions -->
          <div
            class="reply-editor"
            contenteditable={!quickReplySending}
            bind:this={replyEditorRef}
            onkeydown={handleEditorKeydown}
            onpaste={handleEditorPaste}
            oninput={handleEditorInput}
            oncontextmenu={showFormatMenu}
            data-placeholder="Write a reply..."
          ></div>

          <!-- Send button -->
          <button
            class="btn-send"
            onclick={sendQuickReply}
            disabled={quickReplySending}
            title="Send"
          >
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
              <line x1="22" y1="2" x2="11" y2="13" />
              <polygon points="22 2 15 22 11 13 2 9 22 2" />
            </svg>
          </button>
        </div>
      </div>
    {/if}

  {:else}
    <!-- Empty state + FAB -->
    <div class="empty-state">
      {#if mailStore.error}
        <div class="error-box">{mailStore.error}</div>
      {/if}
      <svg width="72" height="72" viewBox="0 0 24 24" fill="none" stroke="var(--text-secondary)" stroke-width="1" opacity="0.4">
        <path d="M4 4h16c1.1 0 2 .9 2 2v12c0 1.1-.9 2-2 2H4c-1.1 0-2-.9-2-2V6c0-1.1.9-2 2-2z" />
        <polyline points="22,6 12,13 2,6" />
      </svg>
      <p class="empty-text">Select a conversation</p>
    </div>

    <button class="compose-fab" onclick={handleCompose} title="New message">
      <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
        <path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7" />
        <path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z" />
      </svg>
    </button>

    {#if showComposer}
      <div class="composer-overlay">
        <Composer mode={null} originalMessage={null} onclose={closeComposer} />
      </div>
    {/if}
  {/if}
</main>

<!-- Format context menu -->
{#if formatMenu}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="format-menu" style:left="{formatMenu.x}px" style:top="{formatMenu.y}px"
    onclick={(e) => e.stopPropagation()}>
    <button onclick={() => execFormat("bold")}><b>Bold</b> <span class="shortcut">Ctrl+B</span></button>
    <button onclick={() => execFormat("italic")}><i>Italic</i> <span class="shortcut">Ctrl+I</span></button>
    <button onclick={() => execFormat("underline")}><u>Underline</u> <span class="shortcut">Ctrl+U</span></button>
    <button onclick={insertLink}>Link...</button>
  </div>
{/if}

<style>
  .chat-view {
    flex: 1;
    display: flex;
    flex-direction: column;
    background: var(--bg-chat);
    min-width: 200px;
    position: relative;
    border-left: 1px solid var(--border-color);
  }

  /* ── Header ── */
  .chat-header {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 8px 16px;
    background: var(--bg-primary);
    border-bottom: 1px solid var(--border-color);
    height: var(--header-height);
  }

  .btn-back {
    width: 36px; height: 36px;
    display: flex; align-items: center; justify-content: center;
    border: none; background: none; border-radius: 50%;
    cursor: pointer; color: var(--text-secondary); flex-shrink: 0;
  }
  .btn-back:hover { background: var(--bg-hover); }

  .chat-info { flex: 1; min-width: 0; }
  .chat-name {
    font-weight: 600; font-size: var(--font-size);
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  }
  .chat-meta {
    display: flex; align-items: center; gap: 6px;
    font-size: var(--font-size-xs); color: var(--text-secondary);
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  }

  .conn-dot {
    width: 6px; height: 6px; border-radius: 50%;
    flex-shrink: 0;
  }
  .conn-dot.connected { background: #7bc862; }
  .conn-dot.connecting { background: #e5ca77; animation: pulse 1.5s infinite; }
  @keyframes pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.3; } }

  .chat-actions { display: flex; gap: 4px; }
  .btn-icon {
    width: 36px; height: 36px;
    display: flex; align-items: center; justify-content: center;
    border: none; background: none; border-radius: 50%;
    cursor: pointer; color: var(--text-secondary);
    transition: background var(--transition);
  }
  .btn-icon:hover { background: var(--bg-hover); }

  /* ── Messages ── */
  .chat-messages {
    flex: 1;
    overflow-y: auto;
    padding: 8px 16px;
    display: flex;
    flex-direction: column;
  }

  .center-status {
    display: flex; align-items: center; justify-content: center;
    flex: 1; color: var(--text-secondary); font-size: var(--font-size-sm);
  }

  .spinner {
    width: 28px; height: 28px;
    border: 3px solid var(--border-color);
    border-top-color: var(--text-accent);
    border-radius: 50%;
    animation: spin 0.7s linear infinite;
  }
  @keyframes spin { to { transform: rotate(360deg); } }

  .date-separator {
    display: flex; align-items: center; justify-content: center;
    padding: 8px 0; margin: 4px 0;
  }
  .date-separator span {
    background: rgba(0, 0, 0, 0.08);
    padding: 3px 10px; border-radius: 10px;
    font-size: 12px; color: var(--text-secondary); font-weight: 500;
  }

  /* ── Scroll FAB ── */
  .scroll-fab {
    position: absolute;
    bottom: 80px; right: 24px;
    width: 40px; height: 40px;
    border-radius: 50%;
    background: var(--bg-primary);
    border: none;
    box-shadow: 0 2px 8px rgba(0,0,0,0.15);
    cursor: pointer;
    display: flex; align-items: center; justify-content: center;
    color: var(--text-secondary);
    transition: transform 0.15s ease, box-shadow 0.15s ease;
    z-index: 10;
  }
  .scroll-fab:hover { transform: scale(1.1); box-shadow: 0 4px 12px rgba(0,0,0,0.2); }

  /* ── Quick reply ── */
  .quick-reply {
    background: var(--bg-primary);
    border-top: 1px solid var(--border-color);
    padding: 6px 12px;
  }
  /* Identity picker */
  .identity-picker {
    position: relative;
    flex-shrink: 0;
  }
  .identity-picker-btn {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 5px 10px;
    border: 1px solid var(--border-color);
    border-radius: 16px;
    background: var(--bg-secondary);
    cursor: pointer;
    font-family: var(--font-family);
    font-size: var(--font-size-xs);
    color: var(--text-primary);
    max-width: 200px;
    transition: border-color var(--transition);
  }
  .identity-picker-btn:hover { border-color: var(--text-accent); }
  .identity-dot {
    width: 8px; height: 8px;
    border-radius: 50%;
    flex-shrink: 0;
  }
  .identity-email {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .identity-chevron {
    flex-shrink: 0;
    transition: transform 0.15s ease;
  }
  .identity-chevron.open { transform: rotate(180deg); }
  .identity-dropdown {
    position: absolute;
    bottom: calc(100% + 4px);
    left: 0;
    min-width: 240px;
    background: var(--bg-primary);
    border: 1px solid var(--border-color);
    border-radius: 10px;
    box-shadow: 0 4px 16px rgba(0, 0, 0, 0.15);
    overflow: hidden;
    z-index: 50;
  }
  .identity-option {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    padding: 8px 12px;
    border: none;
    background: none;
    cursor: pointer;
    font-family: var(--font-family);
    font-size: var(--font-size-sm);
    color: var(--text-primary);
    text-align: left;
  }
  .identity-option:hover { background: var(--bg-hover); }
  .identity-option.selected { font-weight: 600; }
  .identity-option-email {
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .identity-option svg { color: var(--text-accent); flex-shrink: 0; }

  .reply-input-row {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .reply-editor {
    flex: 1;
    padding: 8px 12px;
    border: 1px solid var(--border-color);
    border-radius: 16px;
    font-size: var(--font-size);
    font-family: var(--font-family);
    color: var(--text-primary);
    background: var(--bg-secondary);
    outline: none;
    min-height: 20px;
    max-height: 120px;
    overflow-y: auto;
    line-height: 1.4;
    word-wrap: break-word;
    overflow-wrap: break-word;
  }
  .reply-editor:focus { border-color: var(--text-accent); }
  .reply-editor:empty::before {
    content: attr(data-placeholder);
    color: var(--text-secondary);
    pointer-events: none;
  }
  .reply-editor :global(a) { color: var(--text-accent); text-decoration: underline; }

  /* Format context menu */
  .format-menu {
    position: fixed;
    background: var(--bg-primary);
    border: 1px solid var(--border-color);
    border-radius: 8px;
    box-shadow: 0 4px 12px rgba(0, 0, 0, 0.15);
    z-index: 200;
    overflow: hidden;
    min-width: 160px;
  }
  .format-menu button {
    display: flex; align-items: center; justify-content: space-between;
    width: 100%; padding: 8px 14px;
    border: none; background: none; cursor: pointer;
    font-size: var(--font-size-sm); font-family: var(--font-family);
    color: var(--text-primary); white-space: nowrap;
  }
  .format-menu button:hover { background: var(--bg-hover); }
  .format-menu .shortcut { color: var(--text-secondary); font-size: var(--font-size-xs); margin-left: 16px; }

  .btn-action {
    width: 36px; height: 36px;
    display: flex; align-items: center; justify-content: center;
    border: none; background: none; border-radius: 50%;
    cursor: pointer; color: var(--text-secondary);
    flex-shrink: 0;
  }
  .btn-action:hover { background: var(--bg-hover); }

  .btn-send {
    width: 36px; height: 36px;
    display: flex; align-items: center; justify-content: center;
    background: var(--bg-active); color: white;
    border: none; border-radius: 50%;
    cursor: pointer; flex-shrink: 0;
    transition: opacity var(--transition);
  }
  .btn-send:hover { opacity: 0.9; }
  .btn-send:disabled { opacity: 0.3; cursor: not-allowed; }

  /* ── Empty state ── */
  .empty-state {
    flex: 1;
    display: flex; flex-direction: column;
    align-items: center; justify-content: center;
    gap: 12px;
  }
  .empty-text { color: var(--text-secondary); font-size: 16px; }

  .error-box {
    background: #fee; color: #c00; padding: 12px 20px; border-radius: 8px;
    font-size: var(--font-size-sm); max-width: 80%; text-align: center;
    border: 1px solid #fcc;
  }

  .compose-fab {
    position: absolute;
    bottom: 24px; right: 24px;
    width: 56px; height: 56px;
    border-radius: 50%;
    background: var(--bg-active);
    border: none; color: white;
    box-shadow: 0 4px 12px rgba(65, 159, 217, 0.4);
    cursor: pointer;
    display: flex; align-items: center; justify-content: center;
    transition: transform 0.15s ease, box-shadow 0.15s ease;
    z-index: 10;
  }
  .compose-fab:hover { transform: scale(1.05); box-shadow: 0 6px 16px rgba(65, 159, 217, 0.5); }

  .composer-overlay {
    position: absolute;
    bottom: 0; left: 0; right: 0;
    z-index: 20;
  }
</style>
