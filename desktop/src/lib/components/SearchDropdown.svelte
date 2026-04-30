<script lang="ts">
  import type { MessageEnvelope, Contact } from "../types/mail";
  import { formatDateShort as formatDate } from "../utils/format";

  interface Props {
    query: string;
    contacts: Contact[];
    results: MessageEnvelope[];
    loading: boolean;
    oncomposeNew: (email: string) => void;
    onselectContact: (c: Contact) => void;
    onselectMessage: (msg: MessageEnvelope) => void;
  }
  let { query, contacts, results, loading, oncomposeNew, onselectContact, onselectMessage }: Props = $props();

  // RFC-5322-ish address detection: anything@anything.tld with a dot in the domain.
  const EMAIL_RE = /^[^\s<>"',]+@[^\s<>"',]+\.[^\s<>"',]+$/;
  const trimmed = $derived(query.trim());
  const composeEmail = $derived(EMAIL_RE.test(trimmed) ? trimmed.toLowerCase() : null);
  const empty = $derived(!loading && !composeEmail && contacts.length === 0 && results.length === 0);
</script>

<div class="dropdown">
  {#if composeEmail}
    <button class="result-item compose-row" onclick={() => oncomposeNew(composeEmail)}>
      <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
        <path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7" />
        <path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z" />
      </svg>
      <span>Написать по адресу: <b>{composeEmail}</b></span>
    </button>
  {/if}

  {#if loading && contacts.length === 0 && results.length === 0}
    <div class="status">Searching...</div>
  {:else if empty}
    <div class="status">No results</div>
  {:else}
    {#if contacts.length > 0}
      <div class="section-header">Contacts</div>
      {#each contacts as c (c.email)}
        <button class="result-item" onclick={() => onselectContact(c)}>
          <div class="contact-name">{c.name || c.email}</div>
          {#if c.name}
            <div class="contact-email">{c.email}</div>
          {/if}
        </button>
      {/each}
    {/if}

    {#if results.length > 0}
      <div class="section-header">Messages</div>
      {#each results as msg (msg.uid)}
        <button class="result-item" onclick={() => onselectMessage(msg)}>
          <div class="result-from">{msg.from || msg.from_addr}</div>
          <div class="result-subject">{msg.subject || "(no subject)"}</div>
          <div class="result-date">{formatDate(msg.date_ts)}</div>
        </button>
      {/each}
    {/if}
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
  .result-item:hover { background: var(--bg-hover); }

  .compose-row {
    display: flex;
    align-items: center;
    gap: 10px;
    border-bottom: 1px solid var(--border-color);
    color: var(--text-accent);
    font-size: var(--font-size-sm);
  }
  .compose-row svg { flex-shrink: 0; }

  .contact-name {
    font-size: var(--font-size-sm);
    font-weight: 500;
    color: var(--text-primary);
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
    grid-column: 1 / span 2; grid-row: 1;
  }
  .contact-email {
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
    grid-column: 1 / span 2; grid-row: 2;
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
