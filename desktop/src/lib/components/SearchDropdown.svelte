<script lang="ts">
  import type { MessageEnvelope } from "../types/mail";

  interface Props {
    results: MessageEnvelope[];
    loading: boolean;
    onselect: (msg: MessageEnvelope) => void;
  }
  let { results, loading, onselect }: Props = $props();

  function formatDate(ts: number): string {
    if (!ts) return "";
    const date = new Date(ts * 1000);
    return date.toLocaleDateString([], { day: "numeric", month: "short" });
  }
</script>

<div class="dropdown">
  {#if loading}
    <div class="status">Searching...</div>
  {:else if results.length === 0}
    <div class="status">No results</div>
  {:else}
    <!-- TODO: Contact results section (CardDAV) -->
    <div class="section-header">Messages</div>
    {#each results as msg (msg.uid)}
      <button class="result-item" onclick={() => onselect(msg)}>
        <div class="result-from">{msg.from || msg.from_addr}</div>
        <div class="result-subject">{msg.subject || "(no subject)"}</div>
        <div class="result-date">{formatDate(msg.date_ts)}</div>
      </button>
    {/each}
  {/if}
</div>

<style>
  .dropdown {
    position: absolute;
    top: 100%;
    left: 0;
    right: 0;
    max-height: 400px;
    overflow-y: auto;
    background: var(--bg-primary);
    border: 1px solid var(--border-color);
    border-top: none;
    border-radius: 0 0 12px 12px;
    box-shadow: var(--shadow-md);
    z-index: 100;
  }

  .status {
    padding: 16px;
    text-align: center;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .section-header {
    padding: 6px 12px;
    font-size: var(--font-size-xs);
    font-weight: 600;
    color: var(--text-secondary);
    text-transform: uppercase;
    letter-spacing: 0.5px;
    border-bottom: 1px solid var(--border-color);
  }

  .result-item {
    display: grid;
    grid-template-columns: 1fr auto;
    grid-template-rows: auto auto;
    gap: 0 8px;
    width: 100%;
    padding: 8px 12px;
    border: none;
    background: none;
    cursor: pointer;
    font-family: var(--font-family);
    text-align: left;
    transition: background var(--transition);
  }

  .result-item:hover {
    background: var(--bg-hover);
  }

  .result-from {
    font-size: var(--font-size-sm);
    font-weight: 500;
    color: var(--text-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    grid-column: 1;
    grid-row: 1;
  }

  .result-subject {
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    grid-column: 1;
    grid-row: 2;
  }

  .result-date {
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
    grid-column: 2;
    grid-row: 1;
    white-space: nowrap;
  }
</style>
