<script lang="ts">
  import type { MessageBody } from "../types/mail";

  interface Props {
    message: MessageBody;
    isFirstInGroup: boolean;  // first message from this sender in a row
    isLastInGroup: boolean;   // last message from this sender in a row
  }
  let { message, isFirstInGroup, isLastInGroup }: Props = $props();

  function formatTime(ts: number): string {
    if (!ts) return "";
    return new Date(ts * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }

  function formatSize(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  }

  const hasHtml = $derived(!!message.html);
  let showHtml = $state(true);
  let iframeRef = $state<HTMLIFrameElement | null>(null);

  $effect(() => {
    if (iframeRef && message.html && showHtml) {
      const doc = iframeRef.contentDocument;
      if (doc) {
        doc.open();
        doc.write(`<!DOCTYPE html><html><head><style>
          body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; font-size: 14px; line-height: 1.5; color: #000; margin: 0; padding: 8px; word-wrap: break-word; overflow-x: hidden; }
          img { max-width: 100%; height: auto; }
          a { color: #3390ec; }
          blockquote { border-left: 3px solid #ccc; margin: 8px 0; padding: 4px 12px; color: #555; }
          pre { background: #f5f5f5; padding: 8px; border-radius: 4px; overflow-x: auto; font-size: 13px; }
        </style></head><body>${message.html}</body></html>`);
        doc.close();
      }
    }
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

<div
  class="bubble-wrap"
  class:outgoing={message.is_outgoing}
  class:first={isFirstInGroup}
  class:last={isLastInGroup}
  class:single={isFirstInGroup && isLastInGroup}
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

    {#if hasHtml && showHtml}
      <iframe
        bind:this={iframeRef}
        class="html-frame"
        sandbox="allow-same-origin"
        title="Email content"
      ></iframe>
      <button class="toggle-view" onclick={() => showHtml = false}>Text</button>
    {:else if message.text}
      <div class="text-body">{message.text}</div>
      {#if hasHtml}
        <button class="toggle-view" onclick={() => showHtml = true}>HTML</button>
      {/if}
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

  .html-frame {
    width: 100%;
    min-height: 80px;
    border: none;
    border-radius: 4px;
  }

  .text-body {
    white-space: pre-wrap;
    word-wrap: break-word;
    line-height: 1.45;
    font-size: var(--font-size);
  }

  .text-body.empty {
    color: var(--text-secondary);
    font-style: italic;
  }

  .toggle-view {
    display: inline-block;
    margin-top: 2px;
    padding: 0;
    border: none;
    background: none;
    color: var(--text-link);
    font-size: var(--font-size-xs);
    cursor: pointer;
    font-family: var(--font-family);
  }
  .toggle-view:hover { text-decoration: underline; }

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
</style>
