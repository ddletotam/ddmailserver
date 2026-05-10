import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import type { Account } from "../types/mail";
import type { DesktopCalendar, DesktopCalendarEvent } from "../types/calendar";

// ── Local visibility + colors ──
//
// Source-of-truth for "show this calendar" and "use this color" lives only in
// localStorage — multiple devices intentionally diverge. Hidden set is stored
// rather than visible because the default is "show all" and storing the
// smaller delta means a fresh login starts with everything visible without
// special-casing the empty state.

const HIDDEN_KEY = "ddmail_calendar_hidden";
const COLORS_KEY = "ddmail_calendar_colors";

// 8-color rotation; chosen for both light and dark themes.
const PALETTE = [
  "#3390ec", // blue
  "#e8616a", // red
  "#f0a849", // orange
  "#5fb878", // green
  "#a06cd5", // purple
  "#15adcc", // teal
  "#e87c91", // pink
  "#c8a832", // mustard
];

function loadHidden(): Set<number> {
  try {
    const raw = localStorage.getItem(HIDDEN_KEY);
    return raw ? new Set(JSON.parse(raw)) : new Set();
  } catch {
    return new Set();
  }
}

function saveHidden(hidden: Set<number>) {
  localStorage.setItem(HIDDEN_KEY, JSON.stringify([...hidden]));
}

function loadColors(): Record<number, string> {
  try {
    const raw = localStorage.getItem(COLORS_KEY);
    return raw ? JSON.parse(raw) : {};
  } catch {
    return {};
  }
}

function saveColors(colors: Record<number, string>) {
  localStorage.setItem(COLORS_KEY, JSON.stringify(colors));
}

function defaultColorFor(id: number): string {
  // Bitwise positive index — id may be large, JS numbers handle modulo fine.
  const idx = ((id % PALETTE.length) + PALETTE.length) % PALETTE.length;
  return PALETTE[idx];
}

// ── State ──

let calendars = $state<DesktopCalendar[]>([]);
let events = $state<DesktopCalendarEvent[]>([]);
let loading = $state(false);
let error = $state<string | null>(null);

let hidden = $state<Set<number>>(loadHidden());
let colorOverrides = $state<Record<number, string>>(loadColors());

// Current view window — the consumer sets this and the store re-fetches.
let viewFromMs = $state<number | null>(null);
let viewToMs = $state<number | null>(null);

// Throttle re-fetches that pile up from rapid WS pings.
let _refetchTimer: ReturnType<typeof setTimeout> | null = null;

async function loadEventsNow(account: Account) {
  if (viewFromMs === null || viewToMs === null) return;
  const visibleIds = calendars
    .map((c) => c.id)
    .filter((id) => !hidden.has(id));
  if (visibleIds.length === 0) {
    events = [];
    return;
  }
  try {
    const fresh = await invoke<DesktopCalendarEvent[]>("v2_fetch_calendar_events", {
      accountId: account.id,
      fromMs: viewFromMs,
      toMs: viewToMs,
      calendarIds: visibleIds,
    });
    events = fresh ?? [];
    error = null;
  } catch (e: unknown) {
    error = e instanceof Error ? e.message : String(e);
    console.error("[calendar] fetch events:", e);
  }
}

function scheduleRefetch(account: Account) {
  if (_refetchTimer) clearTimeout(_refetchTimer);
  _refetchTimer = setTimeout(() => {
    _refetchTimer = null;
    loadEventsNow(account);
  }, 500);
}

let _unlistenCalendar: (() => void) | null = null;

export const calendarStore = {
  get calendars() { return calendars; },
  get events() { return events; },
  get loading() { return loading; },
  get error() { return error; },

  isVisible(id: number): boolean {
    return !hidden.has(id);
  },

  colorFor(id: number): string {
    return colorOverrides[id] ?? defaultColorFor(id);
  },

  toggleVisibility(id: number, account: Account) {
    const next = new Set(hidden);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    hidden = next;
    saveHidden(hidden);
    loadEventsNow(account);
  },

  setColor(id: number, color: string) {
    colorOverrides = { ...colorOverrides, [id]: color };
    saveColors(colorOverrides);
  },

  resetColor(id: number) {
    const next = { ...colorOverrides };
    delete next[id];
    colorOverrides = next;
    saveColors(colorOverrides);
  },

  setViewWindow(fromMs: number, toMs: number, account: Account) {
    viewFromMs = fromMs;
    viewToMs = toMs;
    loadEventsNow(account);
  },

  async load(account: Account) {
    loading = true;
    try {
      const fresh = await invoke<DesktopCalendar[]>("v2_list_calendars", {
        accountId: account.id,
      });
      calendars = fresh ?? [];
      error = null;
      // After the calendar list arrives, refresh events for whatever window
      // the consumer already set (most likely the current week on mount).
      await loadEventsNow(account);
    } catch (e: unknown) {
      error = e instanceof Error ? e.message : String(e);
      console.error("[calendar] list calendars:", e);
    } finally {
      loading = false;
    }
  },

  /// Subscribe to push events emitted by the Rust side when the server
  /// publishes a calendar_updated WS frame. Idempotent — safe to call from
  /// each calendar window mount; previous listener is torn down first.
  async startWatching(account: Account) {
    _unlistenCalendar?.();
    _unlistenCalendar = await listen<{ calendar_id: number }>(
      "calendar-updated",
      () => scheduleRefetch(account),
    );
  },

  stopWatching() {
    _unlistenCalendar?.();
    _unlistenCalendar = null;
  },
};

// Re-export for components that want to render a swatch without going
// through the store getter ceremony.
export { defaultColorFor, PALETTE };
