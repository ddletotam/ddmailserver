<script lang="ts">
  import type { Conversation } from "../types/mail";
  import { identityStore } from "../stores/identity.svelte";
  import { formatDate, hashColor, initials, cleanName } from "../utils/format";

  interface Props {
    conversation: Conversation;
    active: boolean;
    pinned: boolean;
    onclick: () => void;
    oncontextmenu: (e: MouseEvent) => void;
  }
  let { conversation, active, pinned, onclick, oncontextmenu }: Props = $props();

  const c = $derived(conversation);
  const cp = $derived(c.counterparts[0]);
  const displayName = $derived(cleanName(c.label));
  const tooltip = $derived(cp?.name && cp.name !== cp.addr ? `${cp.name} <${cp.addr}>` : (cp?.addr ?? displayName));
  const identityColor = $derived.by(() => {
    return identityStore.loaded ? identityStore.colorForEmail(c.received_by) : "transparent";
  });
</script>

<button
  class="conv-item"
  class:active
  class:unread={c.unread_count > 0}
  style:background-color={active ? '' : identityColor}
  title={tooltip}
  {onclick}
  {oncontextmenu}
>
  <!-- Avatar -->
  <div class="avatar-wrap">
    <div class="avatar-initials" style:background={hashColor(c.id)}>
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

    <!-- Line 2: Subject of last message + unread badge -->
    <div class="row-2">
      <span class="subject">{c.last_subject}</span>
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
  .conv-item.active .subject,
  .conv-item.active .date { color: var(--text-on-active); }

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

  .subject {
    font-size: var(--font-size-sm); color: var(--text-secondary);
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap; flex: 1;
  }
  .unread .subject { color: var(--text-primary); }

  .badge {
    background: var(--bg-active); color: white;
    font-size: 11px; font-weight: 600;
    padding: 1px 6px; border-radius: 10px;
    min-width: 18px; text-align: center; flex-shrink: 0;
  }
  .conv-item.active .badge { background: white; color: var(--bg-active); }
</style>
