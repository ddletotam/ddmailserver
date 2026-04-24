import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import type {
  Folder,
  Conversation,
  MessageBody,
  MessageRef,
  MessageEnvelope,
  Account,
} from "../types/mail";

const PINNED_KEY = "ddmail_pinned";

function loadPinned(): Set<string> {
  try {
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
let loading = $state(false);
let loadingMessages = $state(false);
let error = $state<string | null>(null);
let pinnedIds = $state<Set<string>>(loadPinned());

// Connection
let connectionState = $state<"disconnected" | "connecting" | "connected" | "error">("disconnected");
let connectionError = $state<string | null>(null);

// Search
let searchResults = $state<MessageEnvelope[]>([]);
let searchLoading = $state(false);

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

    loadingMessages = true;
    error = null;
    try {
      // 1. Load from cache first (instant)
      const cached = await invoke<MessageBody[]>("load_cached_messages", {
        host: account.imap_host,
        username: account.username,
        messages: conv.messages,
      });
      if (cached.length > 0) {
        conversationMessages = cached;
        loadingMessages = false; // show cached immediately
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
      conversationMessages = fresh;
    } catch (e) {
      if (conversationMessages.length === 0) {
        error = String(e);
      }
    } finally {
      loadingMessages = false;
    }
  },

  closeConversation() {
    activeConversationId = null;
    conversationMessages = [];
  },

  async search(account: Account, query: string) {
    if (!query.trim()) {
      searchResults = [];
      return;
    }
    searchLoading = true;
    try {
      searchResults = await invoke<MessageEnvelope[]>("search_messages", {
        ...imapArgs(account),
        userEmail: account.email,
        query,
      });
    } catch (e) {
      error = String(e);
      searchResults = [];
    } finally {
      searchLoading = false;
    }
  },

  clearSearch() {
    searchResults = [];
  },

  smtpArgs,
  imapArgs,
};
