<script lang="ts">
  import { invoke } from "@tauri-apps/api/core";
  import { open as openDialog } from "@tauri-apps/plugin-dialog";
  import { accountStore } from "../stores/accounts.svelte";
  import { mailStore } from "../stores/mail.svelte";
  import { identityStore } from "../stores/identity.svelte";
  import MessageBubble from "./MessageBubble.svelte";
  import Composer from "./Composer.svelte";
  import { cleanName, sameDay, formatDateSeparator } from "../utils/format";
  import type { OutgoingMessage, MessageBody } from "../types/mail";

  let showComposer = $state(false);
  let replyMode = $state<"reply" | "forward" | null>(null);
  let composerSource = $state<MessageBody | null>(null);
  let composerPrefillTo = $state<string>("");
  let composerFocusField = $state<"to" | "subject" | "body">("to");
  let chatContainer = $state<HTMLDivElement | null>(null);
  let showScrollBtn = $state(false);

  // Quote-reply (Telegram-style) for the inline quick-reply input
  let replyTo = $state<MessageBody | null>(null);

  const conv = $derived(mailStore.activeConversation);
  // Override is_outgoing per the conversation owner: a message is outgoing iff its sender
  // matches the dialog's identity (received_by). Anything else — including mail from one
  // of our other identities — is incoming for THIS conversation.
  const msgs = $derived.by(() => {
    const raw = mailStore.conversationMessages;
    const owner = conv?.received_by?.toLowerCase() ?? "";
    if (!owner) return raw;
    return raw.map(m => {
      const isOut = m.from_addr.toLowerCase() === owner;
      return isOut === m.is_outgoing ? m : { ...m, is_outgoing: isOut };
    });
  });
  const lastMessage = $derived(msgs.length > 0 ? msgs[msgs.length - 1] : null);
  const composerOriginal = $derived(composerSource ?? lastMessage);

  // Map Message-Id → message, so each bubble can resolve its parent (in-reply-to).
  // Skip locally-built optimistic messages (their message_id is `local-…`) so quote
  // links never point at a stub — once the server roundtrip lands they're replaced
  // with the real message anyway.
  const byMessageId = $derived.by(() => {
    const map = new Map<string, MessageBody>();
    for (const m of msgs) {
      if (m.message_id && !m.message_id.startsWith("local-")) map.set(m.message_id, m);
    }
    return map;
  });
  function parentOf(m: MessageBody): MessageBody | null {
    if (!m.in_reply_to) return null;
    return byMessageId.get(m.in_reply_to) ?? null;
  }

  function jumpToMessage(m: MessageBody) {
    if (!chatContainer) return;
    const el = chatContainer.querySelector(
      `[data-msg-uid="${m.uid}"][data-msg-folder="${CSS.escape(m.folder)}"]`
    ) as HTMLElement | null;
    if (!el) return;
    el.scrollIntoView({ behavior: "smooth", block: "center" });
    el.classList.add("flash-highlight");
    setTimeout(() => el.classList.remove("flash-highlight"), 1500);
  }

  // For Ctrl+Up shortcut: walk through incoming messages from newest to oldest
  function pickPreviousIncoming(current: MessageBody | null): MessageBody | null {
    const incoming = msgs.filter(m => !m.is_outgoing);
    if (incoming.length === 0) return null;
    if (!current) return incoming[incoming.length - 1];
    const idx = incoming.findIndex(m => m.uid === current.uid && m.folder === current.folder);
    if (idx <= 0) return incoming[0];
    return incoming[idx - 1];
  }

  function quotePreview(m: MessageBody): string {
    const raw = (m.text ?? m.html ?? "").replace(/<[^>]+>/g, " ").replace(/\s+/g, " ").trim();
    return raw.length > 120 ? raw.slice(0, 117) + "…" : raw;
  }

  function escapeHtml(s: string): string {
    return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
  }

  // Message grouping: compute first/last in sender group
  function isFirstInGroup(i: number): boolean {
    if (i === 0) return true;
    return msgs[i].from_addr !== msgs[i - 1].from_addr;
  }
  function isLastInGroup(i: number): boolean {
    if (i === msgs.length - 1) return true;
    return msgs[i].from_addr !== msgs[i + 1].from_addr;
  }

  // Compose intent from search dropdown ("Compose to: …").
  $effect(() => {
    const intent = mailStore.composeIntent;
    if (!intent) return;
    composerSource = null;
    composerPrefillTo = intent.to;
    composerFocusField = intent.focusField;
    replyMode = null;
    showComposer = true;
    mailStore.setComposeIntent(null);
  });

  // Jump-to-message intent — fired when a search-result message is clicked.
  // Wait until msgs contains the target before scrolling. Drop the intent after
  // a short grace window so a stale jump doesn't latch onto an unrelated reload.
  $effect(() => {
    const intent = mailStore.jumpIntent;
    if (!intent) return;
    if (msgs.length > 0) {
      const found = msgs.find(m => m.folder === intent.folder && m.uid === intent.uid);
      if (found) {
        mailStore.setJumpIntent(null);
        requestAnimationFrame(() => jumpToMessage(found));
        return;
      }
    }
    const timer = setTimeout(() => {
      if (mailStore.jumpIntent === intent) mailStore.setJumpIntent(null);
    }, 4000);
    return () => clearTimeout(timer);
  });

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

  function handleReply() { composerSource = null; replyMode = "reply"; composerPrefillTo = ""; composerFocusField = "body"; showComposer = true; }
  function handleForward() { composerSource = null; replyMode = "forward"; composerPrefillTo = ""; composerFocusField = "to"; showComposer = true; }
  function handleCompose() { composerSource = null; replyMode = null; composerPrefillTo = ""; composerFocusField = "to"; showComposer = true; }
  function closeComposer() { showComposer = false; replyMode = null; composerSource = null; composerPrefillTo = ""; composerFocusField = "to"; }

  // From context menu: quick-reply with quote
  function startQuoteReply(msg: MessageBody) {
    replyTo = msg;
    requestAnimationFrame(() => replyEditorRef?.focus());
  }
  function cancelQuoteReply() { replyTo = null; }

  // From context menu: open Composer in forward mode for a specific message
  function startForward(msg: MessageBody) {
    composerSource = msg;
    replyMode = "forward";
    showComposer = true;
  }

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

  // ── Composer expand/collapse + advanced fields ──
  //
  // Collapsed: the inline reply is paperclip + body + send + chevron-down.
  // Expanded: the chevron flips up and an advanced-fields panel appears
  // above showing identity selector + To/Cc/Bcc/Subject inputs. Empty
  // strings mean "auto-derive from the reply target on send"; once the
  // user types, that override sticks. Fields are reset after each send.
  let composerExpanded = $state(false);
  let composerTo = $state("");
  let composerCc = $state("");
  let composerBcc = $state("");
  let composerSubject = $state("");

  $effect(() => {
    // Pre-fill the advanced fields the first time the user opens the panel
    // for a given reply target. Don't overwrite values the user has already
    // typed (non-empty stays).
    if (!composerExpanded) return;
    const target = replyTo
      ?? mailStore.conversationMessages.filter(m => !m.is_outgoing).at(-1)
      ?? (mailStore.conversationMessages.length
            ? mailStore.conversationMessages[mailStore.conversationMessages.length - 1]
            : null);
    if (!target) return;
    if (!composerTo) composerTo = target.from_addr;
    if (!composerSubject) {
      const base = target.subject || "";
      composerSubject = /^re:/i.test(base) ? base : `Re: ${base}`;
    }
  });

  // Quick-reply attachments — same shape & flow as Composer's, scoped to the
  // inline input. We intentionally duplicate the (small) logic instead of
  // extracting a shared component for now: the surface is two consumers and
  // both are stable. If a third place ever needs the picker, refactor.
  type QrAttachKind = "file" | "image";
  interface QrAttach { path: string; name: string; kind: QrAttachKind; cid?: string }
  let qrAttachments = $state<QrAttach[]>([]);
  let qrPendingPicks = $state<{ paths: string[]; remember: boolean } | null>(null);
  const QR_IMAGE_EXT = /\.(png|jpe?g|gif|webp|bmp|svg)$/i;
  type QrModePref = "ask" | "file" | "image";
  function qrLoadPref(): QrModePref {
    const v = localStorage.getItem("ddmail_image_attachment_mode");
    return v === "file" || v === "image" ? v : "ask";
  }
  let qrModePref = $state<QrModePref>(qrLoadPref());
  let qrCidCounter = 0;
  function qrMakeCid(): string {
    qrCidCounter += 1;
    return `att${Date.now().toString(36)}${qrCidCounter}@dd.local`;
  }
  function qrBasename(p: string): string {
    const m = p.replace(/\\/g, "/").match(/[^/]+$/);
    return m ? m[0] : p;
  }
  async function qrPickAttachment() {
    try {
      const picked = await openDialog({ multiple: true, directory: false });
      if (!picked) return;
      const paths = (Array.isArray(picked) ? picked : [picked])
        .filter((p) => !qrAttachments.some((a) => a.path === p));
      if (paths.length === 0) return;
      const allImages = paths.every((p) => QR_IMAGE_EXT.test(p));
      if (allImages && qrModePref === "ask") {
        qrPendingPicks = { paths, remember: false };
        return;
      }
      const kind: QrAttachKind = allImages && qrModePref === "image" ? "image" : "file";
      qrAddPicks(paths, kind);
    } catch (e) {
      console.error("[chatview] qrPickAttachment failed:", e);
    }
  }
  function qrAddPicks(paths: string[], kind: QrAttachKind) {
    const next: QrAttach[] = paths.map((p) => ({
      path: p, name: qrBasename(p), kind,
      cid: kind === "image" ? qrMakeCid() : undefined,
    }));
    qrAttachments = [...qrAttachments, ...next];
  }
  function qrResolvePending(kind: QrAttachKind) {
    if (!qrPendingPicks) return;
    qrAddPicks(qrPendingPicks.paths, kind);
    if (qrPendingPicks.remember) {
      qrModePref = kind;
      try { localStorage.setItem("ddmail_image_attachment_mode", kind); } catch {}
    }
    qrPendingPicks = null;
  }
  function qrRemoveAttachment(path: string) {
    qrAttachments = qrAttachments.filter((a) => a.path !== path);
  }
  const qrGalleryMode = $derived(
    qrAttachments.length > 0 && qrAttachments.every((a) => a.kind === "image")
  );

  async function sendQuickReply() {
    const account = accountStore.activeAccount;
    const text = getEditorText();
    // Allow sending if there's text OR attachments — pure-image quick replies
    // are a valid telegram-style "send picture without caption" flow.
    if (!account || !conv) return;
    if (!text && qrAttachments.length === 0) return;

    // What we're replying to: the explicitly chosen message, or the last incoming, or the last message overall.
    const target: MessageBody | null = replyTo
      ?? msgs.filter(m => !m.is_outgoing).at(-1)
      ?? lastMessage;
    if (!target) return;

    quickReplySending = true;
    try {
      const fromEmail = (selectedFromEmail || account.email).toLowerCase();
      // Prefer user-typed advanced-field values when expanded; otherwise
      // auto-derive from the reply target (parity with the original
      // collapsed-only behaviour).
      const recipient = composerTo.trim() || target.from_addr;
      const baseSubject = target.subject || "";
      const subject = composerSubject.trim()
        || (/^re:/i.test(baseSubject) ? baseSubject : `Re: ${baseSubject}`);
      const ccList = composerCc.split(",").map(s => s.trim()).filter(Boolean);
      const bccList = composerBcc.split(",").map(s => s.trim()).filter(Boolean);
      const userHtml = getEditorHtml();
      const userBody = userHtml || text.replace(/\n/g, "<br>");
      const quoteHtml = buildQuoteBlock(target);
      const quoteText = buildQuoteText(target);

      // Inline pictures: prepend <img cid:…> tiles before the user body so the
      // recipient sees them as a witрина above the text. File attachments are
      // separate and ride along as multipart/mixed parts.
      const fileAttachs = qrAttachments.filter((a) => a.kind === "file");
      const inlineAttachs = qrAttachments.filter((a) => a.kind === "image" && a.cid);
      const inlineImgsHtml = inlineAttachs
        .map((a) =>
          `<div style="margin: 4px 0;"><img src="cid:${a.cid}" alt="${a.name.replace(/"/g, "&quot;")}" style="max-width: 100%; border-radius: 6px;" /></div>`
        )
        .join("");

      const html = `<div style="font-family:sans-serif;font-size:14px">${inlineImgsHtml}${userBody}${quoteHtml}</div>`;
      const fullText = `${text}\n\n${quoteText}`;
      // Threading headers: In-Reply-To = target's Message-Id; References = target.references + target.message_id.
      const inReplyTo = target.message_id ? `<${target.message_id}>` : null;
      const refIds = [...(target.references ?? []), ...(target.message_id ? [target.message_id] : [])];
      const references = refIds.length > 0 ? refIds.map(id => `<${id}>`).join(" ") : null;

      // Did the user pick a different identity than the conversation's current one?
      const conversationIdentity = conv.received_by.toLowerCase();
      const switchedIdentity = fromEmail !== conversationIdentity;

      // Local optimistic message (only if identity didn't change — otherwise we'll redirect to a different convo).
      const localMsgId = `local-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
      // Recipient list. composerTo overrides target.from_addr (the auto-derived
      // recipient) when set; either way, advanced cc adds extras.
      const toList = composerTo.trim()
        ? composerTo.split(",").map(s => s.trim()).filter(Boolean)
        : [recipient];

      const optimistic: MessageBody = {
        uid: -Date.now(), // negative uid signals "not from server yet"
        folder: "Sent",
        subject,
        from: fromEmail,
        from_addr: fromEmail,
        to: toList,
        cc: [],
        date: new Date().toUTCString(),
        date_ts: Math.floor(Date.now() / 1000),
        html,
        text: fullText,
        attachments: [],
        is_outgoing: true,
        message_id: localMsgId,
        in_reply_to: target.message_id ?? "",
        references: refIds,
      };
      if (!switchedIdentity) {
        mailStore.appendLocalMessage(optimistic);
      }

      // bccList is collected from the advanced field but currently dropped on
      // the floor — OutgoingMessage has no bcc slot yet. TODO: extend the
      // protocol so SMTP envelope picks up Bcc without leaking into headers.
      void bccList;

      const msg: OutgoingMessage = {
        from: fromEmail,
        to: toList,
        cc: ccList,
        subject,
        text: fullText,
        html,
        in_reply_to: inReplyTo,
        references,
        attachment_paths: fileAttachs.map((a) => a.path),
        inline_paths: inlineAttachs.map((a) => ({ path: a.path, content_id: a.cid! })),
      };
      await invoke("v2_send_message", {
        accountId: account.id,
        smtpHost: account.smtp_host,
        smtpPort: account.smtp_port,
        message: msg,
      });
      clearEditor();
      qrAttachments = [];
      replyTo = null;
      // Reset advanced fields after a successful send so the next reply starts
      // fresh. The expanded panel itself stays open if the user opened it —
      // that's a deliberate state, not a per-message thing.
      composerTo = "";
      composerCc = "";
      composerBcc = "";
      composerSubject = "";

      if (switchedIdentity) {
        // Land in the conversation that matches the new (counterpart, identity) pair.
        const cpAddr = (conv.counterparts[0]?.addr ?? recipient).toLowerCase();
        const targetConvId = `${fromEmail}|${cpAddr}`;
        await mailStore.loadConversations(account);
        await mailStore.openConversation(account, targetConvId);
      } else {
        // Quietly reconcile the optimistic message with the server's view in the background.
        setTimeout(() => mailStore.refreshActive(account), 1500);
      }
    } catch (e) {
      console.error("Send failed:", e);
    } finally {
      quickReplySending = false;
    }
  }

  function buildQuoteBlock(m: MessageBody): string {
    const who = escapeHtml(m.from || m.from_addr);
    const when = m.date ? escapeHtml(m.date) : "";
    // Quote the parent as plain text, NOT raw HTML — otherwise scripts / on*-handlers
    // / tracking pixels from the original sender flow through us into the recipient.
    const plain = m.text ?? htmlToText(m.html ?? "");
    const body = escapeHtml(plain).replace(/\n/g, "<br>");
    return `<blockquote style="margin:12px 0 0 0;padding:0 0 0 12px;border-left:3px solid #ccc;color:#666;">`
      + `<div style="font-size:12px;margin-bottom:4px;">${who}${when ? ` &middot; ${when}` : ""}</div>`
      + body + `</blockquote>`;
  }

  function htmlToText(html: string): string {
    if (!html) return "";
    return html
      .replace(/<style[\s\S]*?<\/style>/gi, "")
      .replace(/<script[\s\S]*?<\/script>/gi, "")
      .replace(/<br\s*\/?>/gi, "\n")
      .replace(/<\/(p|div|h[1-6]|li|tr)>/gi, "\n")
      .replace(/<[^>]+>/g, "")
      .replace(/&nbsp;/g, " ")
      .replace(/&amp;/g, "&")
      .replace(/&lt;/g, "<")
      .replace(/&gt;/g, ">")
      .replace(/&quot;/g, '"')
      .replace(/&#39;/g, "'")
      .replace(/[ \t]+\n/g, "\n")
      .replace(/\n{3,}/g, "\n\n")
      .trim();
  }

  function buildQuoteText(m: MessageBody): string {
    const header = `On ${m.date || ""}, ${m.from || m.from_addr} wrote:`;
    const body = (m.text ?? "").split("\n").map(l => `> ${l}`).join("\n");
    return `${header}\n${body}`;
  }


  // Keyboard shortcuts
  function handleKeydown(e: KeyboardEvent) {
    if (e.key === "Escape") {
      if (identityDropdownOpen) { identityDropdownOpen = false; e.preventDefault(); }
      else if (replyTo) { cancelQuoteReply(); e.preventDefault(); }
      else if (showComposer) { closeComposer(); e.preventDefault(); }
      else if (conv) { mailStore.closeConversation(); e.preventDefault(); }
      return;
    }
    // Ctrl+Up: reply to last incoming message; repeated presses step further back.
    if ((e.ctrlKey || e.metaKey) && e.key === "ArrowUp" && conv && !showComposer) {
      const target = pickPreviousIncoming(replyTo);
      if (target) {
        e.preventDefault();
        startQuoteReply(target);
      }
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
            parent={parentOf(msg)}
            onreply={startQuoteReply}
            onforward={startForward}
            onjump={jumpToMessage}
          />
        {/each}
      {/if}
    </div>

    <!-- Scroll to bottom FAB -->
    {#if showScrollBtn && !showComposer}
      <button class="scroll-fab" onclick={scrollToBottom} title="Scroll to bottom">
        <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5">
          <polyline points="6 9 12 15 18 9" />
        </svg>
      </button>
    {/if}

    <!-- Quick-reply input (Telegram-style) -->
    {#if showComposer}
      <Composer mode={replyMode} originalMessage={composerOriginal} prefillTo={composerPrefillTo} focusField={composerFocusField} onclose={closeComposer} />
    {:else}
      <div class="quick-reply">
        {#if composerExpanded}
          <div class="advanced-fields">
            {#if identityStore.hasMultiple}
              <div class="advanced-row">
                <label>From</label>
                <div class="identity-picker advanced-picker">
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
              </div>
            {/if}
            <div class="advanced-row">
              <label for="adv-to">To</label>
              <input id="adv-to" type="text" bind:value={composerTo} placeholder="recipient@example.com" />
            </div>
            <div class="advanced-row">
              <label for="adv-cc">Cc</label>
              <input id="adv-cc" type="text" bind:value={composerCc} placeholder="comma-separated" />
            </div>
            <div class="advanced-row">
              <label for="adv-bcc">Bcc</label>
              <input id="adv-bcc" type="text" bind:value={composerBcc} placeholder="comma-separated" />
            </div>
            <div class="advanced-row">
              <label for="adv-subject">Subject</label>
              <input id="adv-subject" type="text" bind:value={composerSubject} placeholder="Subject" />
            </div>
          </div>
        {/if}

        {#if replyTo}
          <div class="reply-quote-bar">
            <div class="reply-quote-content">
              <div class="reply-quote-name">{replyTo.from || replyTo.from_addr}</div>
              <div class="reply-quote-text">{quotePreview(replyTo)}</div>
            </div>
            <button class="reply-quote-close" onclick={cancelQuoteReply} title="Cancel reply" aria-label="Cancel reply">
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
              </svg>
            </button>
          </div>
        {/if}

        {#if qrPendingPicks}
          <div class="pick-prompt">
            <div class="pick-prompt-text">
              Send {qrPendingPicks.paths.length === 1 ? "this picture" : `${qrPendingPicks.paths.length} pictures`} as…
            </div>
            <div class="pick-prompt-actions">
              <button type="button" class="pick-btn" onclick={() => qrResolvePending("image")}>📷 Pictures</button>
              <button type="button" class="pick-btn" onclick={() => qrResolvePending("file")}>📎 Files</button>
            </div>
            <label class="pick-remember">
              <input type="checkbox" bind:checked={qrPendingPicks.remember} />
              Remember
            </label>
          </div>
        {/if}

        {#if qrAttachments.length > 0}
          {#if qrGalleryMode}
            <div class="gallery-row">
              {#each qrAttachments as att (att.path)}
                <div class="gallery-tile" title={att.name}>
                  <div class="gallery-tile-thumb">
                    <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
                      <rect x="3" y="3" width="18" height="18" rx="2" />
                      <circle cx="8.5" cy="8.5" r="1.5" />
                      <polyline points="21 15 16 10 5 21" />
                    </svg>
                  </div>
                  <span class="gallery-tile-name">{att.name}</span>
                  <button type="button" class="gallery-tile-remove" onclick={() => qrRemoveAttachment(att.path)} aria-label="Remove">
                    <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5">
                      <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
                    </svg>
                  </button>
                </div>
              {/each}
            </div>
          {:else}
            <div class="attachments-row">
              {#each qrAttachments as att (att.path)}
                <div class="attachment-chip" title={att.path}>
                  <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                    <path d="M21.44 11.05l-9.19 9.19a6 6 0 0 1-8.49-8.49l9.19-9.19a4 4 0 0 1 5.66 5.66l-9.2 9.19a2 2 0 0 1-2.83-2.83l8.49-8.48" />
                  </svg>
                  <span class="attachment-name">{att.name}</span>
                  <button type="button" class="attachment-remove" onclick={() => qrRemoveAttachment(att.path)} aria-label="Remove">
                    <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5">
                      <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
                    </svg>
                  </button>
                </div>
              {/each}
            </div>
          {/if}
        {/if}

        <div class="reply-input-row">
          <!-- Attach button: opens system file picker. Image-only batches
               trigger the "send as pictures or files" prompt above. -->
          <button
            type="button"
            class="qr-attach-btn"
            onclick={qrPickAttachment}
            title="Attach files"
            aria-label="Attach files"
          >
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
              <path d="M21.44 11.05l-9.19 9.19a6 6 0 0 1-8.49-8.49l9.19-9.19a4 4 0 0 1 5.66 5.66l-9.2 9.19a2 2 0 0 1-2.83-2.83l8.49-8.48" />
            </svg>
          </button>

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

          <!-- Expand/collapse toggle. Double-chevron flips up when expanded
               so the user has a visual cue. -->
          <button
            type="button"
            class="qr-expand-btn"
            class:open={composerExpanded}
            onclick={() => composerExpanded = !composerExpanded}
            title={composerExpanded ? "Collapse" : "Expand (To/Cc/Subject)"}
            aria-label="Toggle composer fields"
          >
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5">
              <polyline points="7 13 12 18 17 13" />
              <polyline points="7 6 12 11 17 6" />
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
        <Composer mode={null} originalMessage={null} prefillTo={composerPrefillTo} focusField={composerFocusField} onclose={closeComposer} />
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

  :global(.flash-highlight .bubble) {
    animation: flash-bubble 1.4s ease-out;
  }
  @keyframes flash-bubble {
    0%   { box-shadow: 0 0 0 3px var(--text-accent); }
    100% { box-shadow: 0 1px 1px rgba(0, 0, 0, 0.06); }
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

  /* Telegram-style quote bar above the editor */
  .reply-quote-bar {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 6px;
    padding: 6px 8px 6px 10px;
    background: var(--bg-secondary);
    border-left: 3px solid var(--text-accent);
    border-radius: 6px;
  }
  .reply-quote-content {
    flex: 1; min-width: 0;
    display: flex; flex-direction: column; gap: 1px;
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
  .reply-quote-close {
    width: 26px; height: 26px;
    display: flex; align-items: center; justify-content: center;
    border: none; background: none; border-radius: 50%;
    cursor: pointer; color: var(--text-secondary); flex-shrink: 0;
  }
  .reply-quote-close:hover { background: var(--bg-hover); color: var(--text-primary); }
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

  /* Quick-reply attachments — mirrors Composer's gallery/chip styling but
     scoped here to keep the inline reply self-contained. */
  .qr-attach-btn,
  .qr-expand-btn {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 36px;
    height: 36px;
    border: none;
    background: transparent;
    color: var(--text-secondary);
    border-radius: 50%;
    cursor: pointer;
    flex-shrink: 0;
    transition: transform 0.18s ease;
  }
  .qr-attach-btn:hover,
  .qr-expand-btn:hover { background: var(--bg-hover); color: var(--text-primary); }
  /* Double-chevron points down when collapsed (expand cue), flips 180° when
     expanded (collapse cue). */
  .qr-expand-btn.open { transform: rotate(180deg); color: var(--text-accent); }

  /* Advanced fields panel — appears above the input row in expanded mode. */
  .advanced-fields {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 10px 12px 6px;
    border-top: 1px solid var(--border-color);
    background: var(--bg-secondary);
  }
  .advanced-row {
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .advanced-row label {
    width: 56px;
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
    text-align: right;
    flex-shrink: 0;
  }
  .advanced-row input[type="text"] {
    flex: 1;
    min-width: 0;
    padding: 6px 10px;
    border: 1px solid var(--border-color);
    border-radius: 6px;
    background: var(--bg-primary);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    outline: none;
  }
  .advanced-row input[type="text"]:focus {
    border-color: var(--text-accent);
  }
  .advanced-picker { flex: 1; min-width: 0; }

  .attachments-row {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    padding: 6px 12px;
    background: var(--bg-secondary);
    border-top: 1px solid var(--border-color);
  }
  .attachment-chip {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 4px 4px 4px 8px;
    background: var(--bg-primary);
    border: 1px solid var(--border-color);
    border-radius: 14px;
    font-size: var(--font-size-xs);
    color: var(--text-primary);
    max-width: 240px;
  }
  .attachment-name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .attachment-remove {
    width: 18px;
    height: 18px;
    display: flex;
    align-items: center;
    justify-content: center;
    border: none;
    background: none;
    border-radius: 50%;
    cursor: pointer;
    color: var(--text-secondary);
    flex-shrink: 0;
  }
  .attachment-remove:hover { background: var(--bg-hover); color: var(--text-primary); }

  .gallery-row {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    padding: 8px 12px;
    background: var(--bg-secondary);
    border-top: 1px solid var(--border-color);
  }
  .gallery-tile {
    position: relative;
    display: flex;
    flex-direction: column;
    align-items: center;
    width: 84px;
    padding: 8px;
    background: var(--bg-primary);
    border: 1px solid var(--border-color);
    border-radius: 8px;
  }
  .gallery-tile-thumb {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 56px;
    height: 56px;
    color: var(--text-accent);
  }
  .gallery-tile-name {
    width: 100%;
    margin-top: 4px;
    font-size: var(--font-size-xs);
    color: var(--text-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    text-align: center;
  }
  .gallery-tile-remove {
    position: absolute;
    top: 2px; right: 2px;
    width: 18px; height: 18px;
    display: flex;
    align-items: center;
    justify-content: center;
    border: none;
    border-radius: 50%;
    background: var(--bg-secondary);
    color: var(--text-secondary);
    cursor: pointer;
  }
  .gallery-tile-remove:hover { background: var(--bg-hover); color: var(--text-primary); }

  .pick-prompt {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 8px 12px;
    background: var(--bg-secondary);
    border-top: 1px solid var(--border-color);
    font-size: var(--font-size-sm);
    color: var(--text-primary);
  }
  .pick-prompt-text { flex-shrink: 0; }
  .pick-prompt-actions { display: flex; gap: 6px; }
  .pick-btn {
    padding: 4px 10px;
    border: 1px solid var(--border-color);
    border-radius: 14px;
    background: var(--bg-primary);
    color: var(--text-primary);
    cursor: pointer;
    font-size: var(--font-size-xs);
  }
  .pick-btn:hover { background: var(--bg-hover); }
  .pick-remember {
    margin-left: auto;
    display: flex;
    align-items: center;
    gap: 4px;
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
    user-select: none;
    cursor: pointer;
  }
</style>
