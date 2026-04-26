const STORAGE_KEY = "ddmail_content_permissions";

interface PermissionData {
  allowMedia: Record<string, true>;
  allowScripts: Record<string, true>;
  allowDomains: string[];
}

function load(): PermissionData {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (raw) return JSON.parse(raw);
  } catch {}
  return { allowMedia: {}, allowScripts: {}, allowDomains: [] };
}

function save(d: PermissionData) {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(d));
}

let data = $state<PermissionData>(load());

export const permissionStore = {
  isMediaAllowed(addr: string): boolean {
    return !!data.allowMedia[addr.toLowerCase()];
  },

  isScriptsAllowed(addr: string): boolean {
    return !!data.allowScripts[addr.toLowerCase()];
  },

  isDomainAllowed(domain: string): boolean {
    return data.allowDomains.includes(domain.toLowerCase());
  },

  get allowedDomains(): string[] {
    return data.allowDomains;
  },

  toggleMedia(addr: string) {
    const key = addr.toLowerCase();
    const newMedia = { ...data.allowMedia };
    if (newMedia[key]) {
      delete newMedia[key];
    } else {
      newMedia[key] = true;
    }
    data = { ...data, allowMedia: newMedia };
    save(data);
  },

  toggleScripts(addr: string) {
    const key = addr.toLowerCase();
    const newScripts = { ...data.allowScripts };
    if (newScripts[key]) {
      delete newScripts[key];
    } else {
      newScripts[key] = true;
    }
    data = { ...data, allowScripts: newScripts };
    save(data);
  },

  addDomain(domain: string) {
    const key = domain.toLowerCase();
    if (!data.allowDomains.includes(key)) {
      data = { ...data, allowDomains: [...data.allowDomains, key] };
      save(data);
    }
  },

  removeDomain(domain: string) {
    const key = domain.toLowerCase();
    data = { ...data, allowDomains: data.allowDomains.filter(d => d !== key) };
    save(data);
  },
};
