import { listen } from "@tauri-apps/api/event";
import type { Account } from "../types/mail";

const STORAGE_KEY = "ddmail_accounts";

function loadAccounts(): Account[] {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    return raw ? JSON.parse(raw) : [];
  } catch {
    return [];
  }
}

function saveAccounts(accounts: Account[]) {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(accounts));
}

let accounts = $state<Account[]>(loadAccounts());
const initialId = loadAccounts()[0]?.id ?? null;
let activeAccountId = $state<string | null>(initialId);

export const accountStore = {
  get accounts() { return accounts; },
  get activeAccount(): Account | null {
    return accounts.find(a => a.id === activeAccountId) ?? null;
  },
  get activeAccountId() { return activeAccountId; },

  setActive(id: string) {
    activeAccountId = id;
  },

  add(account: Omit<Account, "id">) {
    const id = crypto.randomUUID();
    accounts = [...accounts, { ...account, id }];
    saveAccounts(accounts);
    if (!activeAccountId) activeAccountId = id;
  },

  remove(id: string) {
    accounts = accounts.filter(a => a.id !== id);
    saveAccounts(accounts);
    if (activeAccountId === id) {
      activeAccountId = accounts[0]?.id ?? null;
    }
  },

  update(id: string, data: Partial<Account>) {
    accounts = accounts.map(a => a.id === id ? { ...a, ...data } : a);
    saveAccounts(accounts);
  },
};

// Persist refreshed JWTs from the Tauri backend. NativeProvider auto-refreshes
// on 401 and emits this event so the new token survives the next app restart.
listen<{ account_id: string; token: string }>("token-refreshed", (e) => {
  accountStore.update(e.payload.account_id, { native_token: e.payload.token });
}).catch((err) => console.warn("[accounts] token-refreshed listener failed:", err));
