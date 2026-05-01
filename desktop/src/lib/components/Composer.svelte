<script lang="ts">
  import { invoke } from "@tauri-apps/api/core";
  import { accountStore } from "../stores/accounts.svelte";
  import { mailStore } from "../stores/mail.svelte";
  import { identityStore } from "../stores/identity.svelte";
  import type { MessageBody, OutgoingMessage } from "../types/mail";

  interface Props {
    mode: "reply" | "forward" | null;
    originalMessage: MessageBody | null;
    onclose: () => void;
    prefillTo?: string;
    focusField?: "to" | "subject" | "body";
  }
  let { mode, originalMessage, onclose, prefillTo = "", focusField = "to" }: Props = $props();

  const account = $derived(accountStore.activeAccount);

  // Pre-fill fields based on mode (computed once from props on mount)
  function initTo(): string {
    if (prefillTo) return prefillTo;
    return mode === "reply" && originalMessage ? originalMessage.from_addr : "";
  }
  function initSubject(): string {
    if (mode === "reply" && originalMessage)
      return `Re: ${originalMessage.subject.replace(/^Re:\s*/i, "")}`;
    if (mode === "forward" && originalMessage)
      return `Fwd: ${originalMessage.subject.replace(/^Fwd:\s*/i, "")}`;
    return "";
  }
  function initBody(): string {
    if (mode === "forward" && originalMessage)
      return `\n\n---------- Forwarded message ----------\nFrom: ${originalMessage.from}\nDate: ${originalMessage.date}\nSubject: ${originalMessage.subject}\nTo: ${originalMessage.to.join(", ")}\n\n${originalMessage.text || ""}`;
    if (mode === "reply" && originalMessage)
      return `\n\n${originalMessage.date}, ${originalMessage.from}:\n> ${(originalMessage.text || "").split("\n").join("\n> ")}`;
    return "";
  }

  let to = $state(initTo());
  let cc = $state("");
  let subject = $state(initSubject());
  let bodyText = $state(initBody());

  // From-identity selection. Pre-pick the matching identity for replies (the one
  // that received the parent message), the default identity otherwise, falling
  // back to the account email if no identities are loaded yet.
  function initFrom(): string {
    const acc = accountStore.activeAccount;
    if (mode === "reply" && originalMessage) {
      const replyTarget = originalMessage.is_outgoing
        ? originalMessage.from_addr
        : originalMessage.to.concat(originalMessage.cc).map((s) => extractAddr(s)).find((a) => identityStore.findByEmail(a) !== null);
      if (replyTarget) {
        const id = identityStore.findByEmail(replyTarget);
        if (id) return id.email;
      }
    }
    return identityStore.defaultIdentity?.email ?? identityStore.identities[0]?.email ?? acc?.email ?? "";
  }
  function extractAddr(s: string): string {
    const m = s.match(/<([^>]+)>/);
    return (m ? m[1] : s).trim().toLowerCase();
  }
  let selectedFromEmail = $state(initFrom());
  let identityDropdownOpen = $state(false);

  let sending = $state(false);
  let error = $state("");

  let toInputEl = $state<HTMLInputElement | null>(null);
  let subjectInputEl = $state<HTMLInputElement | null>(null);
  let bodyTextareaEl = $state<HTMLTextAreaElement | null>(null);

  // User-resizable height — drag handle at the top of the composer.
  const HEIGHT_KEY = "ddmail_composer_height";
  function loadHeight(): number {
    const raw = localStorage.getItem(HEIGHT_KEY);
    const n = raw ? parseInt(raw, 10) : NaN;
    return Number.isFinite(n) && n > 200 && n < 2000 ? n : Math.round(window.innerHeight * 0.4);
  }
  let composerHeight = $state(loadHeight());

  function startResize(e: MouseEvent) {
    e.preventDefault();
    const startY = e.clientY;
    const startH = composerHeight;
    const onMove = (ev: MouseEvent) => {
      const delta = startY - ev.clientY; // dragging up grows the composer
      const next = Math.min(Math.max(220, startH + delta), Math.round(window.innerHeight * 0.9));
      composerHeight = next;
    };
    const onUp = () => {
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
      try { localStorage.setItem(HEIGHT_KEY, String(composerHeight)); } catch {}
    };
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
  }

  $effect(() => {
    // Focus the requested field once on mount.
    const target = focusField === "subject" ? subjectInputEl
      : focusField === "body" ? bodyTextareaEl
      : toInputEl;
    target?.focus();
  });

  async function handleSend() {
    if (!account || !to.trim()) return;

    sending = true;
    error = "";

    // Threading headers when replying: In-Reply-To = parent Message-Id, References = chain + parent.
    let inReplyTo: string | null = null;
    let references: string | null = null;
    if (mode === "reply" && originalMessage?.message_id) {
      inReplyTo = `<${originalMessage.message_id}>`;
      const chain = [...(originalMessage.references ?? []), originalMessage.message_id];
      references = chain.map(id => `<${id}>`).join(" ");
    }

    const msg: OutgoingMessage = {
      from: selectedFromEmail || account.email,
      to: to.split(",").map(s => s.trim()).filter(Boolean),
      cc: cc ? cc.split(",").map(s => s.trim()).filter(Boolean) : [],
      subject,
      text: bodyText,
      html: `<div style="font-family: sans-serif; font-size: 14px;">${bodyText.replace(/\n/g, "<br>")}</div>`,
      in_reply_to: inReplyTo,
      references,
    };

    try {
      await invoke("send_message", {
        ...mailStore.smtpArgs(account),
        message: msg,
      });
      onclose();
    } catch (e) {
      error = String(e);
    } finally {
      sending = false;
    }
  }
</script>

<div class="composer" style:height="{composerHeight}px">
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="composer-resizer" onmousedown={startResize} aria-label="Resize composer"></div>
  <div class="composer-header">
    <span class="composer-title">
      {#if mode === "reply"}Reply{:else if mode === "forward"}Forward{:else}New Message{/if}
    </span>
    <button class="btn-close" onclick={onclose} title="Close">
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
        <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
      </svg>
    </button>
  </div>

  <div class="fields">
    {#if identityStore.hasMultiple}
      <div class="field-row">
        <label for="compose-from">From:</label>
        <div class="identity-picker">
          <button
            id="compose-from"
            type="button"
            class="identity-picker-btn"
            onclick={() => identityDropdownOpen = !identityDropdownOpen}
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
                  type="button"
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
      </div>
    {/if}
    <div class="field-row">
      <label for="compose-to">To:</label>
      <input id="compose-to" type="text" bind:value={to} bind:this={toInputEl} placeholder="recipient@example.com" />
    </div>
    <div class="field-row">
      <label for="compose-cc">Cc:</label>
      <input id="compose-cc" type="text" bind:value={cc} placeholder="cc@example.com" />
    </div>
    <div class="field-row">
      <label for="compose-subject">Subject:</label>
      <input id="compose-subject" type="text" bind:value={subject} bind:this={subjectInputEl} placeholder="Subject" />
    </div>
  </div>

  <!-- TODO: Replace with TipTap editor for rich HTML editing -->
  <textarea
    class="body-input"
    bind:value={bodyText}
    bind:this={bodyTextareaEl}
    placeholder="Write a message..."
  ></textarea>

  {#if error}
    <div class="error">{error}</div>
  {/if}

  <div class="composer-footer">
    <div class="toolbar">
      <!-- Future: TipTap formatting buttons -->
      <button class="btn-toolbar" title="Attach file">
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <path d="M21.44 11.05l-9.19 9.19a6 6 0 0 1-8.49-8.49l9.19-9.19a4 4 0 0 1 5.66 5.66l-9.2 9.19a2 2 0 0 1-2.83-2.83l8.49-8.48" />
        </svg>
      </button>
    </div>
    <button class="btn-send" onclick={handleSend} disabled={sending || !to.trim()}>
      {#if sending}
        Sending...
      {:else}
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <line x1="22" y1="2" x2="11" y2="13" />
          <polygon points="22 2 15 22 11 13 2 9 22 2" />
        </svg>
      {/if}
    </button>
  </div>
</div>

<style>
  .composer {
    display: flex;
    flex-direction: column;
    background: var(--bg-primary);
    border-top: 1px solid var(--border-color);
    position: relative;
  }

  .composer-resizer {
    position: absolute;
    top: -3px; left: 0; right: 0;
    height: 6px;
    cursor: ns-resize;
    z-index: 60;
  }
  .composer-resizer::before {
    content: "";
    position: absolute;
    top: 2px; left: 50%;
    transform: translateX(-50%);
    width: 36px; height: 2px;
    border-radius: 1px;
    background: var(--border-color);
    transition: background var(--transition);
  }
  .composer-resizer:hover::before { background: var(--text-accent); }

  .composer-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 8px 16px;
    border-bottom: 1px solid var(--border-color);
  }

  .composer-title {
    font-weight: 600;
    font-size: var(--font-size);
  }

  .btn-close {
    width: 32px;
    height: 32px;
    display: flex;
    align-items: center;
    justify-content: center;
    border: none;
    background: none;
    border-radius: 50%;
    cursor: pointer;
    color: var(--text-secondary);
    transition: background var(--transition);
  }

  .btn-close:hover {
    background: var(--bg-hover);
  }

  .fields {
    padding: 4px 16px;
  }

  .field-row {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 4px 0;
    border-bottom: 1px solid var(--border-color);
  }

  .field-row label {
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
    width: 60px;
    flex-shrink: 0;
  }

  .field-row input {
    flex: 1;
    border: none;
    outline: none;
    font-size: var(--font-size);
    font-family: var(--font-family);
    padding: 4px 0;
  }

  .identity-picker {
    position: relative;
    flex: 1;
  }
  .identity-picker-btn {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 4px 10px;
    border: 1px solid var(--border-color);
    border-radius: 14px;
    background: var(--bg-secondary);
    cursor: pointer;
    font-family: var(--font-family);
    font-size: var(--font-size-sm);
    color: var(--text-primary);
    max-width: 280px;
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
    /* Open UPWARDS so the list never falls off the bottom of the composer/screen. */
    bottom: calc(100% + 4px);
    top: auto;
    left: 0;
    min-width: 240px;
    max-height: 280px;
    overflow-y: auto;
    background: var(--bg-primary);
    border: 1px solid var(--border-color);
    border-radius: 10px;
    box-shadow: 0 4px 16px rgba(0, 0, 0, 0.15);
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

  .body-input {
    flex: 1;
    min-height: 120px;
    padding: 12px 16px;
    border: none;
    outline: none;
    resize: none;
    font-size: var(--font-size);
    font-family: var(--font-family);
    line-height: 1.5;
  }

  .body-input::placeholder {
    color: var(--text-secondary);
  }

  .error {
    padding: 4px 16px;
    color: #d32f2f;
    font-size: var(--font-size-sm);
  }

  .composer-footer {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 8px 16px;
    border-top: 1px solid var(--border-color);
  }

  .toolbar {
    display: flex;
    gap: 4px;
  }

  .btn-toolbar {
    width: 36px;
    height: 36px;
    display: flex;
    align-items: center;
    justify-content: center;
    border: none;
    background: none;
    border-radius: 50%;
    cursor: pointer;
    color: var(--text-secondary);
    transition: background var(--transition);
  }

  .btn-toolbar:hover {
    background: var(--bg-hover);
  }

  .btn-send {
    width: 40px;
    height: 40px;
    display: flex;
    align-items: center;
    justify-content: center;
    background: var(--bg-active);
    color: white;
    border: none;
    border-radius: 50%;
    cursor: pointer;
    transition: opacity var(--transition);
    font-family: var(--font-family);
    font-size: var(--font-size-sm);
  }

  .btn-send:hover {
    opacity: 0.9;
  }

  .btn-send:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
</style>
