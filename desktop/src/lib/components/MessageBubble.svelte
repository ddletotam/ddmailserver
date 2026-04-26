<script lang="ts">
  import { invoke } from "@tauri-apps/api/core";
  import { accountStore } from "../stores/accounts.svelte";
  import type { MessageBody } from "../types/mail";

  interface Props {
    message: MessageBody;
    isFirstInGroup: boolean;  // first message from this sender in a row
    isLastInGroup: boolean;   // last message from this sender in a row
  }
  let { message, isFirstInGroup, isLastInGroup }: Props = $props();

  // Context menu
  let contextMenu = $state<{ x: number; y: number } | null>(null);
  let showSource = $state(false);
  let sourceText = $state("");
  let sourceLoading = $state(false);

  function handleContextMenu(e: MouseEvent) {
    e.preventDefault();
    contextMenu = { x: e.clientX, y: e.clientY };
  }

  function closeContextMenu() {
    contextMenu = null;
  }

  async function viewSource() {
    contextMenu = null;
    const account = accountStore.activeAccount;
    if (!account) return;

    showSource = true;
    sourceLoading = true;
    try {
      sourceText = await invoke<string>("fetch_message_source", {
        host: account.imap_host,
        port: account.imap_port,
        username: account.username,
        password: account.password,
        useTls: account.use_tls,
        folder: message.folder,
        uid: message.uid,
      });
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

  function formatTime(ts: number): string {
    if (!ts) return "";
    return new Date(ts * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }

  function formatSize(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  }

  /** Strip HTML to clean text with minimal formatting */
  function sanitizeHtml(html: string): string {
    let s = html;

    // Remove style blocks and executable blocks
    s = s.replace(/<style[\s\S]*?<\/style>/gi, "");
    const scrTag = "scr" + "ipt";
    s = s.replace(new RegExp(`<${scrTag}[\\s\\S]*?<\\/${scrTag}>`, "gi"), "");
    // Remove HTML comments
    s = s.replace(/<!--[\s\S]*?-->/g, "");

    // Preserve <pre> blocks — extract, replace with placeholders, restore later
    const preBlocks: string[] = [];
    s = s.replace(/<pre[^>]*>[\s\S]*?<\/pre>/gi, (match) => {
      preBlocks.push(match);
      return `%%PRE${preBlocks.length - 1}%%`;
    });

    // Convert <a href="url">text</a> → text (remove links unless bare URL visible in text)
    s = s.replace(/<a[^>]*href="([^"]*)"[^>]*>([\s\S]*?)<\/a>/gi, (_match, href, text) => {
      const cleanText = text.replace(/<[^>]*>/g, "").trim();
      // If link text IS the URL, keep it visible
      if (cleanText === href || cleanText.startsWith("http")) {
        return cleanText;
      }
      // Otherwise just show the text without the link
      return cleanText;
    });

    // Strip all attributes from remaining tags
    s = s.replace(/<(\w+)\s[^>]*?>/g, "<$1>");

    // Remove all tags except allowed ones
    const allowed = new Set(["br", "p", "div", "strong", "b", "em", "i", "pre"]);
    s = s.replace(/<\/?([a-zA-Z][a-zA-Z0-9]*)\s*\/?>/g, (match, tag) => {
      return allowed.has(tag.toLowerCase()) ? match : "";
    });

    // Collapse all whitespace (newlines, spaces, tabs) between tags to single space
    s = s.replace(/>\s+</g, "> <");
    // Remove empty block tags: <p></p>, <div></div>, <p> </p> etc.
    s = s.replace(/<(p|div)>\s*<\/\1>/gi, "");
    // Collapse multiple br to single br
    s = s.replace(/(<br\s*\/?>[\s]*){2,}/gi, "<br>");
    // Remove br right after opening block or before closing block
    s = s.replace(/<(p|div)>\s*<br\s*\/?>/gi, "<$1>");
    s = s.replace(/<br\s*\/?>\s*<\/(p|div)>/gi, "</$1>");
    // Remove leading/trailing br
    s = s.replace(/^(\s*<br\s*\/?>[\s]*)+/i, "");
    s = s.replace(/([\s]*<br\s*\/?>[\s]*)+$/i, "");
    // Collapse remaining whitespace runs (but not inside pre)
    s = s.replace(/\n\s*\n/g, "\n");

    // Restore <pre> blocks
    for (let i = 0; i < preBlocks.length; i++) {
      s = s.replace(`%%PRE${i}%%`, preBlocks[i]);
    }

    return s.trim();
  }

  /** Remove URLs from text that are hidden behind labels in HTML */
  function removeHiddenUrls(text: string, html: string): string {
    // Collect URL prefixes that are hidden in HTML (href behind a label)
    const hiddenPrefixes: string[] = [];
    const linkRe = /<a[^>]*href=["']([^"']*)["'][^>]*>([\s\S]*?)<\/a>/gi;
    let m;
    while ((m = linkRe.exec(html)) !== null) {
      let href = m[1].replace(/&amp;/g, "&");
      const linkText = m[2].replace(/<[^>]*>/g, "").trim();
      if (linkText !== href && !linkText.startsWith("http")) {
        // Use the first 40 chars of URL as prefix for fuzzy matching
        try {
          const u = new URL(href);
          hiddenPrefixes.push(u.origin + u.pathname);
        } catch {
          if (href.length > 20) hiddenPrefixes.push(href.substring(0, 40));
        }
      }
    }
    if (hiddenPrefixes.length === 0) return text;

    // Remove any URL in text that starts with a hidden prefix
    let result = text;
    for (const prefix of hiddenPrefixes) {
      const escaped = prefix.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
      result = result.replace(new RegExp(`${escaped}[^\\s]*`, "g"), "");
    }

    // Clean up leftover empty lines and trailing whitespace
    result = result.replace(/\n\s*\n\s*\n/g, "\n\n");
    return result.trim();
  }

  /** Clean up plain text for display */
  function cleanPlainText(text: string, html: string | null): string {
    let s = text;

    // 1. Markdown images ![alt](url) → remove entirely
    s = s.replace(/!\[[^\]]*\]\([^)]*\)/g, "");

    // 2. Markdown links [text](url) → just text
    s = s.replace(/\[([^\]]*)\]\([^)]*\)/g, "$1");

    // 3. Remove bare URLs that are hidden in HTML
    if (html) {
      s = removeHiddenUrls(s, html);
    }

    // 4. Clean leftover Markdown artifacts
    s = s.replace(/^\s*---+\s*$/gm, "");      // horizontal rules
    s = s.replace(/^\s*\|\s*$/gm, "");         // lonely pipe chars
    s = s.replace(/^\s*\]\s*$/gm, "");         // stray brackets

    // 5. Collapse multiple empty lines to max one
    s = s.replace(/\n{3,}/g, "\n\n");
    s = s.trim();

    return s;
  }

  // Display content: prefer text, fallback to sanitized HTML
  const displayContent = $derived.by(() => {
    if (message.text && message.text.trim()) {
      return { type: "text" as const, content: cleanPlainText(message.text, message.html) };
    }
    if (message.html) {
      return { type: "html" as const, content: sanitizeHtml(message.html) };
    }
    return { type: "empty" as const, content: "" };
  });

  // Sender color (stable per address)
  function senderColor(addr: string): string {
    const colors = ["#e17076", "#7bc862", "#e5ca77", "#65aadd", "#a695e7", "#ee7aae", "#6ec9cb", "#faa774"];
    let hash = 0;
    for (let i = 0; i < addr.length; i++) {
      hash = ((hash << 5) - hash + addr.charCodeAt(i)) | 0;
    }
    return colors[Math.abs(hash) % colors.length];
  }
</script>

<svelte:window onclick={closeContextMenu} />

<div
  class="bubble-wrap"
  class:outgoing={message.is_outgoing}
  class:first={isFirstInGroup}
  class:last={isLastInGroup}
  class:single={isFirstInGroup && isLastInGroup}
  oncontextmenu={handleContextMenu}
>
  {#if isFirstInGroup && !message.is_outgoing}
    <div class="sender-name" style:color={senderColor(message.from_addr)}>
      {message.from}
    </div>
  {/if}

  <div class="bubble" class:outgoing={message.is_outgoing} class:first={isFirstInGroup} class:last={isLastInGroup}>
    {#if message.subject && isFirstInGroup}
      <div class="subject">{message.subject}</div>
    {/if}

    {#if displayContent.type === "text"}
      <div class="text-body text-plain">{displayContent.content}</div>
    {:else if displayContent.type === "html"}
      <div class="text-body text-sanitized">{@html displayContent.content}</div>
    {:else}
      <div class="text-body empty">(empty)</div>
    {/if}

    {#if message.attachments.length > 0}
      <div class="attachments">
        {#each message.attachments as att}
          <div class="attachment">
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
          </div>
        {/each}
      </div>
    {/if}

    <div class="meta-row">
      <span class="time">{formatTime(message.date_ts)}</span>
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

{#if contextMenu}
  <div class="ctx-menu" style:left="{contextMenu.x}px" style:top="{contextMenu.y}px">
    <button onclick={viewSource}>View source</button>
  </div>
{/if}

{#if showSource}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="source-overlay" onclick={closeSource} onkeydown={(e) => e.key === 'Escape' && closeSource()}>
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div class="source-modal" onclick={(e) => e.stopPropagation()}>
      <div class="source-header">
        <span>Message source</span>
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
    max-width: 80%;
    margin-bottom: 1px;
  }

  .bubble-wrap.outgoing {
    align-items: flex-end;
    align-self: flex-end;
  }

  /* Spacing between groups */
  .bubble-wrap.first {
    margin-top: 6px;
  }
  .bubble-wrap.last {
    margin-bottom: 6px;
  }
  .bubble-wrap.single {
    margin-top: 6px;
    margin-bottom: 6px;
  }

  .sender-name {
    font-size: var(--font-size-xs);
    font-weight: 600;
    padding: 0 12px;
    margin-bottom: 1px;
  }

  .bubble {
    background: var(--bg-bubble-incoming);
    padding: 6px 10px;
    box-shadow: 0 1px 1px rgba(0, 0, 0, 0.06);
    overflow: hidden;
    position: relative;
    min-width: 80px;

    /* Grouped corners — Telegram style */
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

  .bubble.outgoing {
    background: var(--bg-bubble-outgoing);
  }

  .subject {
    font-weight: 600;
    font-size: var(--font-size-sm);
    margin-bottom: 2px;
    color: var(--text-primary);
  }

  .text-body :global(p) { margin: 2px 0; }
  .text-body :global(div) { margin: 0; }
  .text-body :global(pre) {
    background: #f5f5f5;
    padding: 8px;
    border-radius: 4px;
    overflow-x: auto;
    font-size: 13px;
    font-family: monospace;
    white-space: pre;
    margin: 4px 0;
  }

  .text-body {
    word-wrap: break-word;
    overflow-wrap: break-word;
    line-height: 1.45;
    font-size: var(--font-size);
  }
  /* Plain text: preserve whitespace */
  .text-body.text-plain {
    white-space: pre-wrap;
  }
  /* Sanitized HTML: normal flow, br/p handle line breaks */
  .text-body.text-sanitized {
    white-space: normal;
  }

  .text-body.empty {
    color: var(--text-secondary);
    font-style: italic;
  }


  .attachments {
    display: flex;
    flex-direction: column;
    gap: 4px;
    margin-top: 6px;
  }

  .attachment {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 10px;
    background: rgba(0, 0, 0, 0.04);
    border-radius: 8px;
    cursor: pointer;
  }
  .attachment:hover { background: rgba(0, 0, 0, 0.07); }

  .att-icon { color: var(--text-accent); flex-shrink: 0; display: flex; }
  .att-info { flex: 1; min-width: 0; }
  .att-name { display: block; font-size: var(--font-size-sm); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .att-size { font-size: var(--font-size-xs); color: var(--text-secondary); }

  .meta-row {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: 3px;
    margin-top: 1px;
  }

  .time {
    font-size: 11px;
    color: var(--text-secondary);
  }

  .checkmark {
    color: var(--text-accent);
    display: flex;
    align-items: center;
  }

  /* Context menu */
  .ctx-menu {
    position: fixed;
    background: var(--bg-primary);
    border: 1px solid var(--border-color);
    border-radius: 8px;
    box-shadow: 0 4px 12px rgba(0, 0, 0, 0.15);
    z-index: 200;
    overflow: hidden;
    min-width: 140px;
  }
  .ctx-menu button {
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
  .ctx-menu button:hover { background: var(--bg-hover); }

  /* Source modal */
  .source-overlay {
    position: fixed;
    inset: 0;
    background: rgba(0, 0, 0, 0.5);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 300;
  }
  .source-modal {
    background: var(--bg-primary);
    border-radius: 12px;
    width: 85vw;
    height: 80vh;
    display: flex;
    flex-direction: column;
    overflow: hidden;
    box-shadow: 0 8px 32px rgba(0, 0, 0, 0.3);
  }
  .source-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 12px 16px;
    border-bottom: 1px solid var(--border-color);
    font-weight: 600;
    font-size: var(--font-size);
  }
  .source-close {
    width: 28px; height: 28px;
    display: flex; align-items: center; justify-content: center;
    border: none; background: none; border-radius: 50%;
    cursor: pointer; color: var(--text-secondary);
  }
  .source-close:hover { background: var(--bg-hover); }
  .source-body {
    flex: 1;
    overflow: auto;
    padding: 12px 16px;
    margin: 0;
    font-family: monospace;
    font-size: 12px;
    line-height: 1.4;
    white-space: pre-wrap;
    word-break: break-all;
    color: var(--text-primary);
    background: var(--bg-secondary);
  }
</style>
