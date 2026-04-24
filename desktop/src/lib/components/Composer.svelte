<script lang="ts">
  import { invoke } from "@tauri-apps/api/core";
  import { accountStore } from "../stores/accounts.svelte";
  import { mailStore } from "../stores/mail.svelte";
  import type { MessageBody, OutgoingMessage } from "../types/mail";

  interface Props {
    mode: "reply" | "forward" | null;
    originalMessage: MessageBody | null;
    onclose: () => void;
  }
  let { mode, originalMessage, onclose }: Props = $props();

  const account = $derived(accountStore.activeAccount);

  // Pre-fill fields based on mode (computed once from props on mount)
  function initTo(): string {
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

  let sending = $state(false);
  let error = $state("");

  async function handleSend() {
    if (!account || !to.trim()) return;

    sending = true;
    error = "";

    const msg: OutgoingMessage = {
      from: account.email,
      to: to.split(",").map(s => s.trim()).filter(Boolean),
      cc: cc ? cc.split(",").map(s => s.trim()).filter(Boolean) : [],
      subject,
      text: bodyText,
      html: `<div style="font-family: sans-serif; font-size: 14px;">${bodyText.replace(/\n/g, "<br>")}</div>`,
      in_reply_to: mode === "reply" && originalMessage ? null : null, // TODO: message-id
      references: null,
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

<div class="composer">
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
    <div class="field-row">
      <label for="compose-to">To:</label>
      <input id="compose-to" type="text" bind:value={to} placeholder="recipient@example.com" />
    </div>
    <div class="field-row">
      <label for="compose-cc">Cc:</label>
      <input id="compose-cc" type="text" bind:value={cc} placeholder="cc@example.com" />
    </div>
    <div class="field-row">
      <label for="compose-subject">Subject:</label>
      <input id="compose-subject" type="text" bind:value={subject} placeholder="Subject" />
    </div>
  </div>

  <!-- TODO: Replace with TipTap editor for rich HTML editing -->
  <textarea
    class="body-input"
    bind:value={bodyText}
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
    max-height: 50vh;
  }

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
