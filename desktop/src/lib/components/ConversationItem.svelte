<script lang="ts">
  import type { Conversation } from "../types/mail";
  import { identityStore } from "../stores/identity.svelte";

  interface Props {
    conversation: Conversation;
    active: boolean;
    pinned: boolean;
    onclick: () => void;
    oncontextmenu: (e: MouseEvent) => void;
  }
  let { conversation, active, pinned, onclick, oncontextmenu }: Props = $props();

  const c = $derived(conversation);

  /** Just strip angle-bracket email from name, nothing else */
  function cleanName(raw: string): string {
    // "Name <email@host>" → "Name"
    const stripped = raw.replace(/<[^>]*>/g, "").trim();
    return stripped || raw;
  }

  const displayName = $derived(cleanName(c.label));

  // Identity color for this conversation (based on which of our emails received it)
  const identityColor = $derived.by(() => {
    return identityStore.loaded ? identityStore.colorForEmail(c.received_by) : "transparent";
  });

  function formatDate(ts: number): string {
    if (!ts) return "";
    const date = new Date(ts * 1000);
    const now = new Date();
    const diff = now.getTime() - date.getTime();
    const dayMs = 86400000;
    if (diff < dayMs && date.getDate() === now.getDate())
      return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    if (diff < 7 * dayMs)
      return date.toLocaleDateString([], { weekday: "short" });
    return date.toLocaleDateString([], { day: "numeric", month: "short" });
  }

  function initials(label: string): string {
    const parts = label.split(/[\s@]+/).filter(Boolean);
    if (parts.length >= 2) return (parts[0][0] + parts[1][0]).toUpperCase();
    return label.substring(0, 2).toUpperCase();
  }

  function avatarColor(id: string): string {
    const colors = [
      "#e17076", "#7bc862", "#e5ca77", "#65aadd",
      "#a695e7", "#ee7aae", "#6ec9cb", "#faa774",
    ];
    let hash = 0;
    for (let i = 0; i < id.length; i++)
      hash = ((hash << 5) - hash + id.charCodeAt(i)) | 0;
    return colors[Math.abs(hash) % colors.length];
  }

  // Gravatar: just use URL directly, browser handles caching
  const gravatarUrl = $derived(
    c.avatar_hash
      ? `https://www.gravatar.com/avatar/${c.avatar_hash}?d=404&s=96`
      : null
  );
  let imgFailed = $state(false);
</script>

<button
  class="conv-item"
  class:active
  class:unread={c.unread_count > 0}
  style:background-color={active ? '' : identityColor}
  {onclick}
  {oncontextmenu}
>
  <!-- Avatar -->
  <div class="avatar-wrap">
    <div class="avatar-initials" style:background={avatarColor(c.id)}>
      {initials(displayName)}
    </div>
  </div>

  <!-- 2 lines -->
  <div class="content">
    <!-- Line 1: Name (bold) + time -->
    <div class="row-1">
      <span class="label">
        {#if pinned}
          <svg class="pin-icon" width="12" height="12" viewBox="0 0 24 24" fill="currentColor"><path d="M16 12V4h1V2H7v2h1v8l-2 2v2h5.2v6h1.6v-6H18v-2l-2-2z"/></svg>
        {/if}
        {displayName}
      </span>
      <span class="date">{formatDate(c.last_date_ts)}</span>
    </div>

    <!-- Line 2: Message preview + unread badge -->
    <div class="row-2">
      <span class="preview">
        {#if c.last_from}<span class="from-tag">{c.last_from}: </span>{/if}{c.last_preview}
      </span>
      {#if c.unread_count > 0}
        <span class="badge">{c.unread_count}</span>
      {/if}
    </div>
  </div>
</button>

<style>
  .conv-item {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    padding: 7px 12px;
    border: none;
    background: none;
    cursor: pointer;
    font-family: var(--font-family);
    text-align: left;
    transition: background-color var(--transition);
  }
  .conv-item:hover { background: var(--bg-hover); }
  .conv-item.active { background: var(--bg-active); }
  .conv-item.active .label,
  .conv-item.active .preview,
  .conv-item.active .date,
  .conv-item.active .from-tag { color: var(--text-on-active); }

  /* Avatar */
  .avatar-wrap {
    width: 46px; height: 46px; flex-shrink: 0;
    border-radius: 50%; overflow: hidden;
  }
  .avatar-img {
    width: 100%; height: 100%; object-fit: cover;
  }
  .avatar-initials {
    width: 100%; height: 100%;
    display: flex; align-items: center; justify-content: center;
    color: white; font-weight: 600; font-size: 15px;
  }

  /* Content */
  .content {
    flex: 1; min-width: 0;
    display: flex; flex-direction: column; gap: 2px;
  }
  .row-1, .row-2 {
    display: flex; align-items: center;
    justify-content: space-between; gap: 8px;
  }

  .label {
    display: flex; align-items: center; gap: 3px;
    font-weight: 600; font-size: var(--font-size);
    color: var(--text-primary);
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap; flex: 1;
  }
  .unread .label { font-weight: 700; }
  .pin-icon { opacity: 0.4; flex-shrink: 0; }

  .date {
    font-size: var(--font-size-xs); color: var(--text-secondary);
    white-space: nowrap; flex-shrink: 0;
  }
  .unread .date { color: var(--text-accent); font-weight: 600; }

  .preview {
    font-size: var(--font-size-sm); color: var(--text-secondary);
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap; flex: 1;
  }
  .from-tag { color: var(--text-primary); font-weight: 500; }
  .unread .preview { color: var(--text-primary); }

  .badge {
    background: var(--bg-active); color: white;
    font-size: 11px; font-weight: 600;
    padding: 1px 6px; border-radius: 10px;
    min-width: 18px; text-align: center; flex-shrink: 0;
  }
  .conv-item.active .badge { background: white; color: var(--bg-active); }
</style>
