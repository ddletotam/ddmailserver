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

// IDLE event listeners
let _unlistenNewMail: (() => void) | null = null;
let _unlistenConnState: (() => void) | null = null;

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

function sortedConversations(): Conversation[] {
  const pinned = conversations
    .filter((c) => pinnedIds.has(c.id))
    .sort((a, b) => b.last_date_ts - a.last_date_ts);
  const unpinned = conversations
    .filter((c) => !pinnedIds.has(c.id))
    .sort((a, b) => b.last_date_ts - a.last_date_ts);
  return [...pinned, ...unpinned];
}

// ── Exports ──

export const mailStore = {
  get folders() {
    return folders;
  },
  get conversations() {
    return sortedConversations();
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

      // 2. Sync from server in background
      folders = await invoke<Folder[]>("connect", imapArgs(account));
      const fresh = await invoke<Conversation[]>("fetch_conversations", {
        ...imapArgs(account),
        userEmail: account.email,
        limit: 200,
      });
      conversations = fresh;

      // 3. Start IDLE watcher
      this._setupIdle(account);
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

    // Listen for new mail events from IDLE
    _unlistenNewMail = await listen<{ folder: string; count: number }>("new-mail", () => {
      // Refresh conversations on new mail
      this.loadConversations(account);
    });

    // Listen for connection state
    _unlistenConnState = await listen<{ state: string; message: string | null }>(
      "connection-state",
      (event) => {
        connectionState = event.payload.state as typeof connectionState;
        connectionError = event.payload.message ?? null;
      },
    );

    // Start the IDLE watcher
    invoke("start_watching", {
      ...imapArgs(account),
      userEmail: account.email,
    }).catch(() => {
      // IDLE not critical — swallow errors
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

      // 2. Fetch fresh from server
      const fresh = await invoke<MessageBody[]>(
        "fetch_conversation_messages",
        {
          ...imapArgs(account),
          userEmail: account.email,
          messages: conv.messages,
        }
      );
      if (activeConversationId !== conversationId) return;
      conversationMessages = fresh;

      // 3. Load draft if exists
      if (conv.draft) {
        try {
          const draftBodies = await invoke<MessageBody[]>(
            "fetch_conversation_messages",
            {
              ...imapArgs(account),
              userEmail: account.email,
              messages: [conv.draft],
            }
          );
          if (activeConversationId === conversationId && draftBodies.length > 0) {
            draftMessage = draftBodies[0];
          }
        } catch {
          // Draft loading failure is non-critical
        }
      }

      // 4. Mark as read (update local count immediately, server in background)
      if (conv.unread_count > 0) {
        conv.unread_count = 0;
        conversations = [...conversations];
        // One IMAP session for the whole conversation, grouped by folder server-side.
        invoke("set_flags_batch", {
          ...imapArgs(account),
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
      const fresh = await invoke<MessageBody[]>("fetch_conversation_messages", {
        ...imapArgs(account),
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
    const messagesPromise = invoke<MessageEnvelope[]>("search_messages", {
      ...imapArgs(account),
      userEmail: account.email,
      query: q,
    }).catch((e) => { console.warn("search_messages:", e); return [] as MessageEnvelope[]; });
    try {
      const [contacts, messages] = await Promise.all([contactsPromise, messagesPromise]);
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
};
