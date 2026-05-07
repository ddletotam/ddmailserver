import { invoke } from "@tauri-apps/api/core";
import type { Identity, Account } from "../types/mail";

// Ugly gray for unknown/alias recipients
const UNKNOWN_COLOR = "#d5d5d0";

let identities = $state<Identity[]>([]);
let loaded = $state(false);

export const identityStore = {
  get identities() { return identities; },
  get loaded() { return loaded; },
  get hasMultiple() { return identities.length > 1; },

  /** Get the default identity */
  get defaultIdentity(): Identity | null {
    return identities.find(i => i.is_default) ?? identities[0] ?? null;
  },

  /** Find identity by email (case-insensitive) */
  findByEmail(email: string): Identity | null {
    const lower = email.toLowerCase();
    return identities.find(i => i.email.toLowerCase() === lower) ?? null;
  },

  /** Get color for a recipient email — identity color or ugly gray for aliases */
  colorForEmail(email: string): string {
    const id = this.findByEmail(email);
    return id?.color ?? UNKNOWN_COLOR;
  },

  /** Determine which identity received a conversation (from TO/CC of incoming messages) */
  identityForConversation(toAddrs: string[]): Identity | null {
    for (const addr of toAddrs) {
      const id = this.findByEmail(addr);
      if (id) return id;
    }
    return null;
  },

  /** Load identities from server (via IMAP METADATA or native HTTP API) */
  async load(account: Account) {
    try {
      identities = await invoke<Identity[]>("v2_fetch_identities", {
        accountId: account.id,
        host: account.imap_host,
        username: account.username,
      });
      loaded = true;
    } catch (e) {
      console.warn("[identity] Failed to load:", e);
      identities = [];
      loaded = true;
    }
  },
};
