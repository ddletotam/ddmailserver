import { invoke } from "@tauri-apps/api/core";
import { accountStore } from "./accounts.svelte";

// Memoized in-flight lookups + resolved data URLs. The Tauri side has its
// own SQLite cache so disk persistence is handled there; this layer just
// avoids invoking the command twice for the same email within one session.
const dataUrlByEmail = new Map<string, string>();
const inflight = new Map<string, Promise<string>>();

// Reactivity bridge: a component reads `avatarStore.urlFor(email)`; when the
// fetch resolves we bump this counter to invalidate $derived consumers. Map
// mutations don't trigger Svelte reactivity on their own, so we keep an
// auxiliary state value that *is* reactive.
let bumped = $state(0);

async function load(email: string): Promise<string> {
  const key = email.trim().toLowerCase();
  if (!key) return "";
  if (dataUrlByEmail.has(key)) return dataUrlByEmail.get(key)!;
  const existing = inflight.get(key);
  if (existing) return existing;

  const account = accountStore.activeAccount;
  if (!account) {
    console.warn("[avatar] no active account, skipping", key);
    return "";
  }
  console.log("[avatar] fetching", key);

  const promise = (async () => {
    try {
      const res = await invoke<{ data: string; mime: string }>("v2_fetch_avatar", {
        accountId: account.id,
        email: key,
      });
      console.log("[avatar] result for", key, "mime=", res.mime, "bytes=", res.data?.length ?? 0);
      // Real MIME from the source — `data:image/*;base64,...` doesn't render
      // in Chromium, the spec requires a concrete type.
      const url = res.data && res.mime ? `data:${res.mime};base64,${res.data}` : "";
      dataUrlByEmail.set(key, url);
      bumped++;
      return url;
    } catch (e) {
      console.warn("[avatar] fetch failed for", key, e);
      dataUrlByEmail.set(key, "");
      bumped++;
      return "";
    } finally {
      inflight.delete(key);
    }
  })();
  inflight.set(key, promise);
  return promise;
}

export const avatarStore = {
  /// Returns the data URL if already resolved, "" if known to be empty, and
  /// triggers a lazy fetch otherwise. Components re-read this in a $derived
  /// block — when the fetch resolves, the dependency on `bumped` re-runs.
  urlFor(email: string): string {
    void bumped; // subscribe to reactivity bridge
    const key = email.trim().toLowerCase();
    if (!key) return "";
    if (dataUrlByEmail.has(key)) return dataUrlByEmail.get(key)!;
    // Kick off async load; current render gets "" and a subsequent re-run
    // gets the resolved URL once the map is populated.
    void load(key);
    return "";
  },
};
