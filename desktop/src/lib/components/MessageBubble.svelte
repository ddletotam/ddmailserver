<script lang="ts">
  import { accountStore } from "../stores/accounts.svelte";
  import { permissionStore } from "../stores/permissions.svelte";
  import { themeStore } from "../stores/theme.svelte";
  import { fetchMessageSource, downloadAttachment } from "../api/imap";
  import { formatDateTime, formatSize, hashColor } from "../utils/format";
  import { resolveDisplayContent, extractBlockedDomains, extractEmailDomains, type DisplayMode, type ContentPermissions } from "../utils/html";
  import { t } from "../i18n/index.svelte";
  import SandboxedEmail from "./SandboxedEmail.svelte";
  import type { MessageBody, Attachment } from "../types/mail";

  interface Props {
    message: MessageBody;
    isFirstInGroup: boolean;
    isLastInGroup: boolean;
    parent?: MessageBody | null;
    onreply?: (msg: MessageBody) => void;
    onforward?: (msg: MessageBody) => void;
    onjump?: (msg: MessageBody) => void;
  }
  let { message, isFirstInGroup, isLastInGroup, parent = null, onreply, onforward, onjump }: Props = $props();

  const parentPreview = $derived.by(() => {
    if (!parent) return "";
    const raw = (parent.text ?? parent.html ?? "").replace(/<[^>]+>/g, " ").replace(/\s+/g, " ").trim();
    return raw.length > 100 ? raw.slice(0, 97) + "…" : raw;
  });
  const parentName = $derived(parent ? (parent.from || parent.from_addr) : "");

  // Display mode: auto prefers HTML, user can switch
  let displayMode = $state<DisplayMode>("auto");
  const hasMultipart = $derived(!!(message.html && message.text));

  // Context menu
  let contextMenu = $state<{ x: number; y: number } | null>(null);
  let showMediaSubmenu = $state(false);
  let showImagesSubmenu = $state(false);
  let showScriptsSubmenu = $state(false);
  let showSource = $state(false);
  let sourceText = $state("");
  let sourceLoading = $state(false);

  // Permission state — read reactive values here so $derived tracks them
  // Domains referenced in this message's HTML
  const blockedDomains = $derived(message.html ? extractBlockedDomains(message.html) : []);
  const emailDomains = $derived(message.html ? extractEmailDomains(message.html) : { imageDomains: [], scriptDomains: [], allDomains: [] });

  const contentPermissions: ContentPermissions = $derived({
    mediaAllowed: loadAllOnce || permissionStore.isMediaAllowed(message.from_addr),
    scriptsAllowed: loadAllOnce || permissionStore.isScriptsAllowed(message.from_addr),
    allowedDomains: permissionStore.allowedDomains,
  });
  const mediaAllowed = $derived(contentPermissions.mediaAllowed);
  const scriptsAllowed = $derived(contentPermissions.scriptsAllowed);

  function handleContextMenu(e: MouseEvent) {
    e.preventDefault();
    showMediaSubmenu = false;
    const menuW = 280, menuH = 300;
    const x = Math.min(e.clientX, window.innerWidth - menuW);
    const y = Math.min(e.clientY, window.innerHeight - menuH);
    contextMenu = { x, y };
  }

  function closeContextMenu() {
    contextMenu = null;
    showMediaSubmenu = false;
    showImagesSubmenu = false;
    showScriptsSubmenu = false;
  }

  async function viewSource() {
    contextMenu = null;
    const account = accountStore.activeAccount;
    if (!account) return;

    showSource = true;
    sourceLoading = true;
    try {
      sourceText = await fetchMessageSource(account, message.folder, message.uid);
    } catch (e) {
      sourceText = `Error: ${e}`;
    } finally {
      sourceLoading = false;
    }
  }

  function closeSource() {
    showSource = false;
    sourceText = "";
  }

  function toggleDisplayMode() {
    contextMenu = null;
    displayMode = displayMode === "text" ? "html" : "text";
  }

  // "Load all" for this message — remembered per component instance
  let loadAllOnce = $state(false);
  function loadAllInMessage() {
    loadAllOnce = true;
    contextMenu = null;
  }

  function toggleMedia() {
    permissionStore.toggleMedia(message.from_addr);
  }

  function toggleScripts() {
    permissionStore.toggleScripts(message.from_addr);
  }

  let downloadingIndex = $state<number | null>(null);
  async function openAttachment(att: Attachment) {
    const account = accountStore.activeAccount;
    if (!account || downloadingIndex !== null) return;
    downloadingIndex = att.index;
    try {
      await downloadAttachment(account, message.folder, message.uid, att.index, att.filename);
    } catch (e) {
      console.error("attachment download/open failed:", e);
    } finally {
      downloadingIndex = null;
    }
  }

  const displayContent = $derived(resolveDisplayContent(message.text, message.html, displayMode));
</script>

<svelte:window onclick={closeContextMenu} />

<div
  class="bubble-wrap"
  class:outgoing={message.is_outgoing}
  class:first={isFirstInGroup}
  class:last={isLastInGroup}
  class:single={isFirstInGroup && isLastInGroup}
  class:has-html={displayContent.type === "html"}
  data-msg-uid={message.uid}
  data-msg-folder={message.folder}
  oncontextmenu={handleContextMenu}
>
  <div class="bubble" class:outgoing={message.is_outgoing} class:first={isFirstInGroup} class:last={isLastInGroup}>
    {#if parent}
      <button
        type="button"
        class="reply-quote"
        onclick={(e) => { e.stopPropagation(); onjump?.(parent); }}
        title="Jump to original message"
      >
        <span class="reply-quote-bar"></span>
        <span class="reply-quote-body">
          <span class="reply-quote-name">{parentName}</span>
          <span class="reply-quote-text">{parentPreview}</span>
        </span>
      </button>
    {/if}
    {#if message.subject}
      <div class="subject">{message.subject}</div>
    {/if}

    {#if displayContent.type === "html"}
      <SandboxedEmail
        html={displayContent.content}
        isDark={themeStore.isDark}
        permissions={contentPermissions}
      />
    {:else if displayContent.type === "text"}
      <div class="text-body text-plain">{displayContent.content}</div>
    {:else}
      <div class="text-body empty">{t("empty")}</div>
    {/if}

    {#if message.attachments.length > 0}
      <div class="attachments">
        {#each message.attachments as att}
          <button
            type="button"
            class="attachment"
            class:loading={downloadingIndex === att.index}
            disabled={downloadingIndex !== null && downloadingIndex !== att.index}
            onclick={() => openAttachment(att)}
            title={att.filename}
          >
            <div class="att-icon">
              <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
                <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
                <polyline points="14 2 14 8 20 8" />
              </svg>
            </div>
            <div class="att-info">
              <span class="att-name">{att.filename}</span>
              <span class="att-size">{formatSize(att.size)}</span>
            </div>
          </button>
        {/each}
      </div>
    {/if}

    <div class="meta-row">
      <span class="time">{formatDateTime(message.date_ts)}</span>
      {#if message.is_outgoing}
        <span class="checkmark" title="Sent">
          <svg width="16" height="11" viewBox="0 0 16 11">
            <path d="M1 5.5L5.5 10L14.5 1" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>
          </svg>
        </span>
      {/if}
    </div>
  </div>
</div>

<!-- Context menu -->
{#if contextMenu}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="ctx-menu" style:left="{contextMenu.x}px" style:top="{contextMenu.y}px"
    onclick={(e) => e.stopPropagation()}>
    <button onclick={() => { closeContextMenu(); onreply?.(message); }}>{t("menu.reply")}</button>
    <button onclick={() => { closeContextMenu(); onforward?.(message); }}>{t("menu.forward")}</button>
    <div class="ctx-divider"></div>

    {#if hasMultipart}
      <button onclick={toggleDisplayMode}>
        {displayMode === "text" ? t("menu.viewAsHtml") : t("menu.viewAsText")}
      </button>
    {/if}

    <button onclick={viewSource}>{t("menu.viewSource")}</button>

    {#if message.html && emailDomains.allDomains.length > 0}
      <button onclick={() => { showMediaSubmenu = !showMediaSubmenu; showImagesSubmenu = false; showScriptsSubmenu = false; }}>
        {showMediaSubmenu ? "\u25BE" : "\u25B8"} {t("menu.mediaElements")}
      </button>

      {#if showMediaSubmenu}
        <button class="ctx-sub-item" onclick={loadAllInMessage}>
          {t("menu.loadAll")}
        </button>
        <button class="ctx-sub-item" onclick={() => {
          if (!mediaAllowed) { toggleMedia(); closeContextMenu(); }
          else { toggleMedia(); }
        }}>
          {mediaAllowed ? "\u2611" : "\u2610"} {t("menu.allowAllFrom", message.from_addr)}
        </button>
        {#each emailDomains.allDomains as domain}
          <button class="ctx-sub-item" onclick={() => {
            const wasAllowed = permissionStore.isDomainAllowed(domain);
            if (wasAllowed) permissionStore.removeDomain(domain);
            else { permissionStore.addDomain(domain); closeContextMenu(); }
          }}>
            {permissionStore.isDomainAllowed(domain) ? "\u2611" : "\u2610"} {t("menu.allowAllFrom", domain)}
          </button>
        {/each}

        {#if emailDomains.imageDomains.length > 0}
          <button class="ctx-sub-item" onclick={() => { showImagesSubmenu = !showImagesSubmenu; showScriptsSubmenu = false; }}>
            {showImagesSubmenu ? "\u25BE" : "\u25B8"} {t("menu.images")}
          </button>
          {#if showImagesSubmenu}
            <button class="ctx-sub-item2" onclick={toggleMedia}>
              {mediaAllowed ? "\u2611" : "\u2610"} {t("menu.from", message.from_addr)}
            </button>
            {#each emailDomains.imageDomains as domain}
              <button class="ctx-sub-item2" onclick={() => {
                if (permissionStore.isDomainAllowed(domain)) permissionStore.removeDomain(domain);
                else permissionStore.addDomain(domain);
              }}>
                {permissionStore.isDomainAllowed(domain) ? "\u2611" : "\u2610"} {t("menu.from", domain)}
              </button>
            {/each}
          {/if}
        {/if}

        {#if emailDomains.scriptDomains.length > 0}
          <button class="ctx-sub-item" onclick={() => { showScriptsSubmenu = !showScriptsSubmenu; showImagesSubmenu = false; }}>
            {showScriptsSubmenu ? "\u25BE" : "\u25B8"} {t("menu.scripts")}
          </button>
          {#if showScriptsSubmenu}
            <button class="ctx-sub-item2" onclick={toggleScripts}>
              {scriptsAllowed ? "\u2611" : "\u2610"} {t("menu.from", message.from_addr)}
            </button>
            {#each emailDomains.scriptDomains as domain}
              <button class="ctx-sub-item2" onclick={() => {
                if (permissionStore.isDomainAllowed(domain)) permissionStore.removeDomain(domain);
                else permissionStore.addDomain(domain);
              }}>
                {permissionStore.isDomainAllowed(domain) ? "\u2611" : "\u2610"} {t("menu.from", domain)}
              </button>
            {/each}
          {/if}
        {/if}
      {/if}
    {/if}
  </div>
{/if}

<!-- Source modal -->
{#if showSource}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="source-overlay" onclick={closeSource} onkeydown={(e) => e.key === 'Escape' && closeSource()}>
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div class="source-modal" onclick={(e) => e.stopPropagation()}>
      <div class="source-header">
        <span>{t("source.title")}</span>
        <button class="source-close" onclick={closeSource}>
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
            <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
          </svg>
        </button>
      </div>
      <pre class="source-body">{#if sourceLoading}Loading...{:else}{sourceText}{/if}</pre>
    </div>
  </div>
{/if}

<style>
  .bubble-wrap {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    max-width: 85%;
    min-width: 25%;
    margin-bottom: 6px;
  }
  /* HTML emails: stretch bubble to full available width */
  .bubble-wrap.has-html {
    width: 85%;
    align-items: stretch;
  }

  .bubble-wrap.outgoing {
    align-items: flex-end;
    align-self: flex-end;
  }

  .bubble {
    background: var(--bg-bubble-incoming);
    padding: 6px 10px;
    box-shadow: 0 1px 1px rgba(0, 0, 0, 0.06);
    overflow: visible;
    position: relative;
    min-width: 80px;
    border-radius: 12px;
  }

  /* Incoming bubble corners */
  .bubble:not(.outgoing).first { border-top-left-radius: 12px; border-bottom-left-radius: 4px; }
  .bubble:not(.outgoing):not(.first):not(.last) { border-top-left-radius: 4px; border-bottom-left-radius: 4px; }
  .bubble:not(.outgoing).last { border-top-left-radius: 4px; border-bottom-left-radius: 12px; }
  .bubble:not(.outgoing).first.last { border-top-left-radius: 12px; border-bottom-left-radius: 12px; }

  /* Outgoing bubble corners */
  .bubble.outgoing.first { border-top-right-radius: 12px; border-bottom-right-radius: 4px; }
  .bubble.outgoing:not(.first):not(.last) { border-top-right-radius: 4px; border-bottom-right-radius: 4px; }
  .bubble.outgoing.last { border-top-right-radius: 4px; border-bottom-right-radius: 12px; }
  .bubble.outgoing.first.last { border-top-right-radius: 12px; border-bottom-right-radius: 12px; }

  .bubble.outgoing { background: var(--bg-bubble-outgoing); }

  .subject {
    font-weight: 600;
    font-size: var(--font-size-sm);
    margin-bottom: 2px;
    color: var(--text-primary);
  }

  .reply-quote {
    display: flex;
    align-items: stretch;
    gap: 8px;
    width: 100%;
    margin-bottom: 4px;
    padding: 4px 8px 4px 0;
    border: none;
    background: rgba(0, 0, 0, 0.04);
    border-radius: 6px;
    cursor: pointer;
    text-align: left;
    font-family: var(--font-family);
    color: var(--text-primary);
    overflow: hidden;
  }
  .reply-quote:hover { background: rgba(0, 0, 0, 0.08); }
  .reply-quote-bar {
    width: 3px;
    background: var(--text-accent);
    border-radius: 2px;
    flex-shrink: 0;
  }
  .reply-quote-body {
    display: flex; flex-direction: column; gap: 1px;
    flex: 1; min-width: 0;
    padding: 2px 0;
  }
  .reply-quote-name {
    font-size: var(--font-size-xs);
    font-weight: 600;
    color: var(--text-accent);
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  }
  .reply-quote-text {
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  }

  .text-body {
    word-wrap: break-word;
    overflow-wrap: break-word;
    line-height: 1.45;
    font-size: var(--font-size);
  }
  .text-body.text-plain { white-space: pre-wrap; }
  .text-body.empty { color: var(--text-secondary); font-style: italic; }

  .attachments { display: flex; flex-direction: column; gap: 4px; margin-top: 6px; }
  .attachment {
    display: flex; align-items: center; gap: 8px;
    padding: 8px 10px; background: rgba(0, 0, 0, 0.04);
    border: none; border-radius: 8px; cursor: pointer;
    width: 100%; text-align: left;
    font-family: var(--font-family); color: var(--text-primary);
    transition: background-color var(--transition);
  }
  .attachment:hover:not(:disabled) { background: rgba(0, 0, 0, 0.07); }
  .attachment:disabled { cursor: default; opacity: 0.6; }
  .attachment.loading { opacity: 0.7; }
  .att-icon { color: var(--text-accent); flex-shrink: 0; display: flex; }
  .att-info { flex: 1; min-width: 0; }
  .att-name { display: block; font-size: var(--font-size-sm); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .att-size { font-size: var(--font-size-xs); color: var(--text-secondary); }

  .meta-row { display: flex; align-items: center; justify-content: flex-end; gap: 3px; margin-top: 1px; }
  .time { font-size: 11px; color: var(--text-secondary); }
  .checkmark { color: var(--text-accent); display: flex; align-items: center; }

  /* Context menu */
  .ctx-menu {
    position: fixed; background: var(--bg-primary);
    border: 1px solid var(--border-color); border-radius: 8px;
    box-shadow: 0 4px 12px rgba(0, 0, 0, 0.15);
    z-index: 200; overflow: visible; min-width: 200px;
  }
  .ctx-menu button {
    display: block; width: 100%; padding: 8px 16px;
    border: none; background: none; cursor: pointer;
    font-size: var(--font-size-sm); font-family: var(--font-family);
    text-align: left; white-space: nowrap; color: var(--text-primary);
  }
  .ctx-menu button:hover { background: var(--bg-hover); }
  .ctx-menu button:first-child { border-radius: 8px 8px 0 0; }
  .ctx-menu button:last-child { border-radius: 0 0 8px 8px; }

  .ctx-sub-item {
    padding-left: 28px !important;
    font-size: var(--font-size-xs) !important;
    color: var(--text-secondary) !important;
  }
  .ctx-sub-item:hover { color: var(--text-primary) !important; }
  .ctx-sub-item2 {
    padding-left: 44px !important;
    font-size: var(--font-size-xs) !important;
    color: var(--text-secondary) !important;
  }
  .ctx-sub-item2:hover { color: var(--text-primary) !important; }
  .ctx-separator {
    height: 1px;
    background: var(--border-color);
    margin: 4px 8px;
  }
  .ctx-divider {
    height: 1px;
    background: var(--border-color);
    margin: 4px 0;
  }

  /* Source modal */
  .source-overlay {
    position: fixed; inset: 0; background: rgba(0, 0, 0, 0.5);
    display: flex; align-items: center; justify-content: center; z-index: 300;
  }
  .source-modal {
    background: var(--bg-primary); border-radius: 12px;
    width: 85vw; height: 80vh; display: flex; flex-direction: column;
    overflow: hidden; box-shadow: 0 8px 32px rgba(0, 0, 0, 0.3);
  }
  .source-header {
    display: flex; align-items: center; justify-content: space-between;
    padding: 12px 16px; border-bottom: 1px solid var(--border-color);
    font-weight: 600; font-size: var(--font-size);
  }
  .source-close {
    width: 28px; height: 28px; display: flex; align-items: center; justify-content: center;
    border: none; background: none; border-radius: 50%;
    cursor: pointer; color: var(--text-secondary);
  }
  .source-close:hover { background: var(--bg-hover); }
  .source-body {
    flex: 1; overflow: auto; padding: 12px 16px; margin: 0;
    font-family: monospace; font-size: 12px; line-height: 1.4;
    white-space: pre-wrap; word-break: break-all;
    color: var(--text-primary); background: var(--bg-secondary);
  }
</style>
