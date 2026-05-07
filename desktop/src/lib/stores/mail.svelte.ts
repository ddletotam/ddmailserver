import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import type {
  Folder,
  Conversation,
  MessageBody,
  MessageRef,
  MessageEnvelope,
  Contact,
  Account,
} from "../types/mail";
import { identityStore } from "./identity.svelte";

// Bumped from "ddmail_pinned" when conversation IDs switched from raw counterpart
// addresses to "{my_identity}|{counterpart}" — old pins no longer match anything,
// so we drop the legacy key on first read of v2.
const PINNED_KEY = "ddmail_pinned_v2";
const LEGACY_PINNED_KEY = "ddmail_pinned";

function loadPinned(): Set<string> {
  try {
    if (localStorage.getItem(LEGACY_PINNED_KEY) !== null && localStorage.getItem(PINNED_KEY) === null) {
      localStorage.removeItem(LEGACY_PINNED_KEY);
    }
    const raw = localStorage.getItem(PINNED_KEY);
    return raw ? new Set(JSON.parse(raw)) : new Set();
  } catch {
    return new Set();
  }
}

function savePinned(pinned: Set<string>) {
  localStorage.setItem(PINNED_KEY, JSON.stringify([...pinned]));
}

// ── State ──

let folders = $state<Folder[]>([]);
let conversations = $state<Conversation[]>([]);
let activeConversationId = $state<string | null>(null);
let conversationMessages = $state<MessageBody[]>([]);
let draftMessage = $state<MessageBody | null>(null);
let loading = $state(false);
let loadingMessages = $state(false);
let error = $state<string | null>(null);
let pinnedIds = $state<Set<string>>(loadPinned());

// Connection
let connectionState = $state<"disconnected" | "connecting" | "connected" | "error">("disconnected");
let connectionError = $state<string | null>(null);

// Search — combined contacts + messages, capped at 25 entries total.
const SEARCH_TOTAL_LIMIT = 25;
let searchContacts = $state<Contact[]>([]);
let searchResults = $state<MessageEnvelope[]>([]);
let searchLoading = $state(false);

// Compose intent — set by Sidebar (e.g. "Compose to: …"); ChatView opens Composer with prefill.
export interface ComposeIntent { to: string; focusField: "to" | "subject" | "body"; }
let composeIntent = $state<ComposeIntent | null>(null);

// Jump-to-message intent — set by Sidebar when a search-result message is clicked;
// ChatView scrolls to it after opening the conversation.
let jumpIntent = $state<{ folder: string; uid: number } | null>(null);

// IDLE/push event listeners
let _unlistenNewMail: (() => void) | null = null;
let _unlistenConnState: (() => void) | null = null;
let _idleSetUp = false; // Guard: set up push only once per account

// ── Sorted conversations ──
//
// Sort on every read. The previous version cached the sorted result keyed by
// reference equality on `conversations` and `pinnedIds`, but Svelte 5 wraps
// $state values in a proxy whose identity behaviour around reassignment is
// not guaranteed — the cache silently returned a stale ordering after a new-
// mail push. With ~200 rows the sort is sub-millisecond, so the cache was
// negative ROI: pure correctness risk for a perf saving nobody will measure.
function getSortedConversations(): Conversation[] {
  const pinned = conversations
    .filter((c) => pinnedIds.has(c.id))
    .sort((a, b) => b.last_date_ts - a.last_date_ts);
  const unpinned = conversations
    .filter((c) => !pinnedIds.has(c.id))
    .sort((a, b) => b.last_date_ts - a.last_date_ts);
  return [...pinned, ...unpinned];
}

// ── Helpers ──

function imapArgs(account: Account) {
  return {
    host: account.imap_host,
    port: account.imap_port,
    username: account.username,
    password: account.password,
    useTls: account.use_tls,
  };
}

function smtpArgs(account: Account) {
  return {
    host: account.smtp_host,
    port: account.smtp_port,
    username: account.username,
    password: account.password,
    useTls: account.use_tls,
  };
}

// Track which accounts have been activated in this session
const activatedAccounts = new Set<string>();

/** Activate provider for this account (detect DDMail server, login if native). */
async function ensureActivated(account: Account): Promise<void> {
  if (activatedAccounts.has(account.id)) return;

  // Auto-detect DDMail server if not yet detected
  if (!account.provider_type) {
    try {
      const result = await invoke<{ server_url: string; api_base: string } | null>(
        "detect_server",
        { host: account.imap_host }
      );
      if (result) {
        // DDMail server — login to get JWT
        const token = await invoke<string>("native_login", {
          serverUrl: result.server_url,
          username: account.username,
          password: account.password,
        });
        account.provider_type = "native";
        account.native_url = result.server_url;
        account.native_token = token;
      } else {
        account.provider_type = "imap";
      }
    } catch {
      account.provider_type = "imap";
    }
  }

  // Register provider in Tauri backend
  await invoke<string>("activate_account", {
    accountId: account.id,
    imapHost: account.imap_host,
    imapPort: account.imap_port,
    username: account.username,
    password: account.password,
    useTls: account.use_tls,
    email: account.email,
    nativeUrl: account.native_url ?? null,
    nativeToken: account.native_token ?? null,
  });

  activatedAccounts.add(account.id);
}

// Debounce: coalesce rapid new-mail events into a single loadConversations
let _refreshTimer: ReturnType<typeof setTimeout> | null = null;
let _refreshing = false;

function scheduleRefresh(account: Account) {
  if (_refreshTimer) clearTimeout(_refreshTimer);
  _refreshTimer = setTimeout(async () => {
    _refreshTimer = null;
    if (_refreshing) return; // already in progress
    _refreshing = true;
    try {
      await mailStore.loadConversations(account);
      // If a conversation is open, the conversations list now has the new
      // MessageRef but conversationMessages is still the previous fetch.
      // Re-pull the active thread's bodies so the right pane shows the
      // freshly arrived message instead of stalling until reopen.
      await mailStore.refreshActive(account);
    } finally {
      _refreshing = false;
    }
  }, 2000); // 2s debounce window
}

/// Collapse search-result messages so each (counterpart, my_identity) pair shows up
/// at most once — multiple messages in the same thread otherwise produce a wall of
/// near-identical "Re:"-rows. Keeps the most recent representative per pair.
function dedupByConversation(messages: MessageEnvelope[], account: Account): MessageEnvelope[] {
  const ourLc = new Set<string>(
    identityStore.identities.map((i) => i.email.toLowerCase()).concat([account.email.toLowerCase()])
  );
  const best = new Map<string, MessageEnvelope>();
  for (const m of messages) {
    const fromLc = m.from_addr.toLowerCase();
    const isOut = ourLc.has(fromLc);
    const cp = isOut
      ? (m.to_addrs.find((a) => !ourLc.has(a.toLowerCase())) ?? m.to_addrs[0] ?? "").toLowerCase()
      : fromLc;
    const myId = isOut
      ? fromLc
      : (m.to_addrs.concat(m.cc_addrs).find((a) => ourLc.has(a.toLowerCase()))
          ?? account.email).toLowerCase();
    if (!cp) continue;
    const key = `${myId}|${cp}`;
    const prev = best.get(key);
    if (!prev || prev.date_ts < m.date_ts) best.set(key, m);
  }
  return [...best.values()].sort((a, b) => b.date_ts - a.date_ts);
}

// ── Exports ──

export const mailStore = {
  get folders() {
    return folders;
  },
  get conversations() {
    return getSortedConversations();
  },
  get activeConversationId() {
    return activeConversationId;
  },
  get activeConversation(): Conversation | null {
    return conversations.find((c) => c.id === activeConversationId) ?? null;
  },
  get conversationMessages() {
    return conversationMessages;
  },
  get draftMessage(): MessageBody | null {
    return draftMessage;
  },
  get loading() {
    return loading;
  },
  get loadingMessages() {
    return loadingMessages;
  },
  get error() {
    return error;
  },
  get searchResults() {
    return searchResults;
  },
  get searchContacts() {
    return searchContacts;
  },
  get searchLoading() {
    return searchLoading;
  },
  get connectionState() {
    return connectionState;
  },
  get connectionError() {
    return connectionError;
  },

  isPinned(id: string): boolean {
    return pinnedIds.has(id);
  },

  togglePin(id: string) {
    const next = new Set(pinnedIds);
    if (next.has(id)) {
      next.delete(id);
    } else {
      next.add(id);
    }
    pinnedIds = next;
    savePinned(pinnedIds);
  },

  async loadConversations(account: Account) {
    loading = true;
    error = null;
    try {
      // 1. Load from cache first (instant)
      const cached = await invoke<Conversation[]>("load_cached_conversations", {
        host: account.imap_host,
        username: account.username,
      });
      if (cached.length > 0) {
        conversations = cached;
        loading = false; // show cached data immediately
      }

      // 2. Activate provider (auto-detect DDMail server on first call)
      await ensureActivated(account);

      // 3. Sync folders + conversations in parallel
      const [foldersResult, fresh] = await Promise.all([
        invoke<Folder[]>("v2_list_folders", { accountId: account.id }),
        invoke<Conversation[]>("v2_fetch_conversations", {
          accountId: account.id,
          host: account.imap_host,
          username: account.username,
          userEmail: account.email,
          limit: 200,
        }),
      ]);
      folders = foldersResult;
      conversations = fresh;

      // 4. Start push listener once (not on every load)
      if (!_idleSetUp) {
        this._setupIdle(account);
        _idleSetUp = true;
      }
    } catch (e) {
      // If we have cached data, don't show error for sync failure
      if (conversations.length === 0) {
        error = String(e);
      }
    } finally {
      loading = false;
    }
  },

  async _setupIdle(account: Account) {
    // Clean up old listeners
    _unlistenNewMail?.();
    _unlistenConnState?.();

    // Listen for new mail events — debounced to prevent thundering herd
    _unlistenNewMail = await listen<{ folder: string; count: number }>("new-mail", () => {
      scheduleRefresh(account);
    });

    // Listen for connection state
    _unlistenConnState = await listen<{ state: string; message: string | null }>(
      "connection-state",
      (event) => {
        connectionState = event.payload.state as typeof connectionState;
        connectionError = event.payload.message ?? null;
      },
    );

    // Start the push listener (IDLE for IMAP, WebSocket for native)
    invoke("v2_start_watching", {
      accountId: account.id,
    }).catch(() => {
      // Push not critical — swallow errors
    });
  },

  async openConversation(account: Account, conversationId: string) {
    activeConversationId = conversationId;
    const conv = conversations.find((c) => c.id === conversationId);
    if (!conv) return;

    // Clear previous messages immediately to avoid stale content
    conversationMessages = [];
    draftMessage = null;
    loadingMessages = true;
    error = null;
    try {
      // 1. Load from cache first (instant)
      const cached = await invoke<MessageBody[]>("load_cached_messages", {
        host: account.imap_host,
        username: account.username,
        messages: conv.messages,
      });
      // Bail if user switched to another conversation while loading
      if (activeConversationId !== conversationId) return;
      if (cached.length > 0) {
        conversationMessages = cached;
        loadingMessages = false;
      }

      // 2. Fetch fresh messages + draft in parallel
      const fetchArgs = {
        accountId: account.id,
        host: account.imap_host,
        username: account.username,
        userEmail: account.email,
      };
      const messagesPromise = invoke<MessageBody[]>(
        "v2_fetch_conversation_messages",
        { ...fetchArgs, messages: conv.messages }
      );
      const draftPromise = conv.draft
        ? invoke<MessageBody[]>(
            "v2_fetch_conversation_messages",
            { ...fetchArgs, messages: [conv.draft] }
          ).catch(() => [] as MessageBody[])
        : Promise.resolve([] as MessageBody[]);

      const [fresh, draftBodies] = await Promise.all([messagesPromise, draftPromise]);
      if (activeConversationId !== conversationId) return;
      conversationMessages = fresh;
      if (draftBodies.length > 0) {
        draftMessage = draftBodies[0];
      }

      // 3. Mark as read (update local count immediately, server in background)
      if (conv.unread_count > 0) {
        conv.unread_count = 0;
        conversations = [...conversations];
        invoke("v2_set_flags_batch", {
          accountId: account.id,
          messages: conv.messages,
          flags: "\\Seen",
          add: true,
        }).catch(() => {});
      }
    } catch (e) {
      if (activeConversationId === conversationId && conversationMessages.length === 0) {
        error = String(e);
      }
    } finally {
      if (activeConversationId === conversationId) {
        loadingMessages = false;
      }
    }
  },

  closeConversation() {
    activeConversationId = null;
    conversationMessages = [];
    draftMessage = null;
  },

  /** Append an optimistic locally-built message to the open conversation (pre-server confirmation). */
  appendLocalMessage(msg: MessageBody) {
    conversationMessages = [...conversationMessages, msg];
  },

  /** Re-fetch the open conversation's messages from server (used to reconcile after optimistic send). */
  async refreshActive(account: Account) {
    const id = activeConversationId;
    if (!id) return;
    const conv = conversations.find((c) => c.id === id);
    if (!conv) return;
    try {
      const fresh = await invoke<MessageBody[]>("v2_fetch_conversation_messages", {
        accountId: account.id,
        host: account.imap_host,
        username: account.username,
        userEmail: account.email,
        messages: conv.messages,
      });
      if (activeConversationId === id) conversationMessages = fresh;
    } catch {
      // ignore — keep local view
    }
  },

  async search(account: Account, query: string) {
    const q = query.trim();
    if (!q) {
      searchResults = [];
      searchContacts = [];
      return;
    }
    searchLoading = true;
    // Run contacts and messages in parallel; cap combined output at SEARCH_TOTAL_LIMIT.
    const contactsPromise = invoke<Contact[]>("search_contacts", {
      host: account.imap_host,
      username: account.username,
      query: q,
      limit: SEARCH_TOTAL_LIMIT,
    }).catch((e) => { console.warn("search_contacts:", e); return [] as Contact[]; });
    const messagesPromise = invoke<MessageEnvelope[]>("v2_search_messages", {
      accountId: account.id,
      userEmail: account.email,
      query: q,
    }).catch((e) => { console.warn("search_messages:", e); return [] as MessageEnvelope[]; });
    try {
      const [contacts, rawMessages] = await Promise.all([contactsPromise, messagesPromise]);
      const messages = dedupByConversation(rawMessages, account);
      // Reserve at least ~15 slots for messages when both sections have results,
      // so a noisy contact list never starves the message search.
      const contactCap = messages.length > 0 ? Math.min(10, contacts.length) : Math.min(SEARCH_TOTAL_LIMIT, contacts.length);
      searchContacts = contacts.slice(0, contactCap);
      const remaining = Math.max(0, SEARCH_TOTAL_LIMIT - searchContacts.length);
      searchResults = messages.slice(0, remaining);
    } finally {
      searchLoading = false;
    }
  },

  clearSearch() {
    searchResults = [];
    searchContacts = [];
  },

  get composeIntent() { return composeIntent; },
  setComposeIntent(intent: ComposeIntent | null) { composeIntent = intent; },

  get jumpIntent() { return jumpIntent; },
  setJumpIntent(intent: { folder: string; uid: number } | null) { jumpIntent = intent; },

  smtpArgs,
  imapArgs,
  ensureActivated,
};
