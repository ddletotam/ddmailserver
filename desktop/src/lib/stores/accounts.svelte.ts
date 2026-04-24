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
