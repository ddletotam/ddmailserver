<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import { RRule } from "rrule";
  import { accountStore } from "../stores/accounts.svelte";
  import { mailStore } from "../stores/mail.svelte";
  import { calendarStore, PALETTE } from "../stores/calendar.svelte";
  import { identityStore } from "../stores/identity.svelte";
  import { invoke } from "@tauri-apps/api/core";
  import { listen, type UnlistenFn } from "@tauri-apps/api/event";
  import { getCurrentWebviewWindow } from "@tauri-apps/api/webviewWindow";
  import EventDetail from "./EventDetail.svelte";
  import EventEdit from "./EventEdit.svelte";
  import type { DesktopCalendarEvent } from "../types/calendar";

  // Lower bound for an hour row. Below this, hour labels start overlapping
  // and 15-minute event chips become unclickable. The actual rendered hour
  // height stretches above this whenever the grid viewport has slack.
  const HOUR_HEIGHT_MIN = 60;

  // ── View prefs (persisted in localStorage) ──
  //
  // workdaysOnly: Mon-Fri vs full week.
  // showNonWorkHours: 8:00-18:00 vs 0:00-24:00.

  const DAYS_KEY = "ddmail_calendar_workdays_only";
  const HOURS_KEY = "ddmail_calendar_show_nonwork";

  function loadPref(key: string, def: boolean): boolean {
    const raw = localStorage.getItem(key);
    return raw === null ? def : raw === "1";
  }
  function savePref(key: string, v: boolean) {
    try { localStorage.setItem(key, v ? "1" : "0"); } catch {}
  }

  let workdaysOnly = $state(loadPref(DAYS_KEY, true));
  let showNonWorkHours = $state(loadPref(HOURS_KEY, false));

  const dayCount = $derived(workdaysOnly ? 5 : 7);
  const startHour = $derived(showNonWorkHours ? 0 : 8);
  const endHour = $derived(showNonWorkHours ? 24 : 18);
  const hourCount = $derived(endHour - startHour);
  const hours = $derived(
    Array.from({ length: hourCount }, (_, i) => startHour + i),
  );

  // Measured viewport of the scrollable body. We stretch hour rows to fill
  // it whenever there's room — so the grid no longer leaves dead space at
  // the bottom on tall windows. When the viewport is shorter than
  // hourCount * HOUR_HEIGHT_MIN, we fall back to the minimum and the body
  // scrolls.
  let bodyHeightPx = $state(0);
  const hourHeightPx = $derived(
    Math.max(HOUR_HEIGHT_MIN, Math.floor(bodyHeightPx / Math.max(1, hourCount))),
  );
  const totalHeightPx = $derived(hourCount * hourHeightPx);
  // Pixels per minute — used to convert event start/duration (stored as
  // minutes) into vertical pixel offsets and to translate drag-Y back into
  // minute deltas.
  const pxPerMin = $derived(hourHeightPx / 60);

  function toggleWorkdays() {
    workdaysOnly = !workdaysOnly;
    savePref(DAYS_KEY, workdaysOnly);
  }
  function toggleNonWorkHours() {
    showNonWorkHours = !showNonWorkHours;
    savePref(HOURS_KEY, showNonWorkHours);
  }

  function startOfWeek(d: Date): Date {
    const dow = d.getDay();          // Sun=0..Sat=6
    const monOffset = (dow + 6) % 7; // Mon=0..Sun=6
    const monday = new Date(d);
    monday.setDate(d.getDate() - monOffset);
    monday.setHours(0, 0, 0, 0);
    return monday;
  }

  function endOfWeek(d: Date): Date {
    const e = new Date(d);
    e.setDate(d.getDate() + dayCount); // exclusive end
    e.setHours(0, 0, 0, 0);
    return e;
  }

  let weekStart = $state(startOfWeek(new Date()));

  const days = $derived(
    Array.from({ length: dayCount }, (_, i) => {
      const d = new Date(weekStart);
      d.setDate(weekStart.getDate() + i);
      return d;
    }),
  );

  function isToday(d: Date): boolean {
    const t = new Date();
    return (
      d.getFullYear() === t.getFullYear() &&
      d.getMonth() === t.getMonth() &&
      d.getDate() === t.getDate()
    );
  }

  function fmtDay(d: Date): string {
    return d.toLocaleDateString(undefined, { weekday: "short", day: "numeric", month: "short" });
  }
  function fmtMonthYear(d: Date): string {
    return d.toLocaleDateString(undefined, { month: "long", year: "numeric" });
  }

  function prevWeek() {
    const d = new Date(weekStart);
    d.setDate(d.getDate() - 7);
    weekStart = d;
  }
  function nextWeek() {
    const d = new Date(weekStart);
    d.setDate(d.getDate() + 7);
    weekStart = d;
  }
  function thisWeek() {
    weekStart = startOfWeek(new Date());
  }

  // ── Account / data wiring ──

  let initError = $state<string | null>(null);

  // Re-tick the "now" line every 30 seconds. The line moves smoothly enough
  // at that rate (≈ 0.5 px on a typical 60 px hour) and we don't burn cycles
  // on layout for sub-second precision nobody perceives.
  let nowTs = $state(Date.now());
  let nowTimer: ReturnType<typeof setInterval> | null = null;
  onMount(async () => {
    nowTimer = setInterval(() => { nowTs = Date.now(); }, 30_000);
    const account = accountStore.activeAccount;
    if (!account) {
      initError = "Нет активной учётки. Сначала войдите в основном окне.";
      return;
    }
    try {
      await mailStore.ensureActivated(account);
      // Identity list has to be loaded for "this attendee is me" matching
      // (used by EventDetail's RSVP pills). Main window loads it in
      // App.svelte's effect, but the calendar window skips that branch — so
      // load it explicitly here. Without this myAttendee never resolves and
      // the RSVP pills never render.
      await identityStore.load(account);
      await calendarStore.load(account);
      await calendarStore.startWatching(account);
    } catch (e: unknown) {
      initError = e instanceof Error ? e.message : String(e);
    }
  });

  onDestroy(() => {
    if (nowTimer) clearInterval(nowTimer);
    if (unlistenOpenEvent) unlistenOpenEvent();
    calendarStore.stopWatching();
    invoke("plugin:window-state|save_window_state").catch(() => {});
  });

  // Vertical pixel offset of the current-time line inside a day column.
  // null when "now" falls outside the visible hour range — we render
  // nothing rather than clamping to the top/bottom edge (a stuck line at
  // 18:00 reads as "still working at 18:00" which is misleading).
  const nowOffsetPx = $derived.by((): number | null => {
    const d = new Date(nowTs);
    const min = d.getHours() * 60 + d.getMinutes() + d.getSeconds() / 60 - startHour * 60;
    if (min < 0 || min > hourCount * 60) return null;
    return min * pxPerMin;
  });

  // Re-fetch events whenever the visible week changes (after load() has run
  // at least once — the store ignores setViewWindow when the calendar list
  // is still empty).
  $effect(() => {
    const account = accountStore.activeAccount;
    if (!account) return;
    if (calendarStore.calendars.length === 0) return;
    const from = weekStart.getTime();
    const to = endOfWeek(weekStart).getTime();
    calendarStore.setViewWindow(from, to, account);
  });

  // ── Event positioning ──

  interface PlacedEvent {
    ev: DesktopCalendarEvent;
    occStart: number; // ms — actual instance start (differs from ev.dtstart for RRULE)
    occEnd: number;   // ms — actual instance end
    dayIndex: number;
    // Position stored as minutes-from-start-hour; the renderer multiplies by
    // pxPerMin so the grid can stretch vertically without re-placing.
    topMin: number;
    heightMin: number;
    color: string;
    col: number;   // 0..cols-1 — horizontal slot within the overlap cluster
    cols: number;  // how many slots wide the cluster is (≥1)
  }

  /// Expand an RRULE event's occurrences that overlap [from, to). Returns
  /// the list of start timestamps (ms). When parsing fails (malformed rule,
  /// rare iCal extensions) we fall back to the master DTSTART so the user
  /// still sees *something* on that one date — better than silently dropping.
  function expandRRule(ev: DesktopCalendarEvent, from: number, to: number): number[] {
    if (!ev.rrule) return [ev.dtstart];
    try {
      const opts = RRule.parseString(ev.rrule);
      opts.dtstart = new Date(ev.dtstart);
      const rule = new RRule(opts);
      const occs = rule.between(new Date(from), new Date(to), true);
      let starts = occs.map((d) => d.getTime());
      // Filter out deleted single occurrences (EXDATE values from the master
      // VEVENT). The server delivers them as ms timestamps — a recurring
      // event keeps its UID when the user deletes "just this one" so we'd
      // otherwise still render the cancelled slot.
      if (ev.exdates && ev.exdates.length) {
        const blocked = new Set(ev.exdates);
        starts = starts.filter((ms) => !blocked.has(ms));
      }
      return starts;
    } catch (e) {
      console.warn("[calendar] RRULE parse failed", ev.uid, ev.rrule, e);
      return [ev.dtstart];
    }
  }

  function placeEvents(): PlacedEvent[] {
    const placed: PlacedEvent[] = [];
    const minView = weekStart.getTime();
    const maxView = endOfWeek(weekStart).getTime();
    const duration = (ev: DesktopCalendarEvent) =>
      (ev.dtend ?? ev.dtstart + 60 * 60 * 1000) - ev.dtstart;

    for (const ev of calendarStore.events) {
      if (ev.all_day) continue; // all-day band not implemented yet

      // For non-recurring events, the master dtstart IS the occurrence.
      // For recurring, expand within the current view window so a weekly
      // standup created last year appears every week.
      const occurrences = ev.rrule
        ? expandRRule(ev, minView, maxView)
        : (ev.dtstart < maxView && (ev.dtend ?? ev.dtstart) > minView ? [ev.dtstart] : []);

      const dur = duration(ev);

      for (const occStart of occurrences) {
        const occEnd = occStart + dur;
        if (occStart >= maxView || occEnd <= minView) continue;

        const startDate = new Date(occStart);
        const dayMidnight = new Date(startDate);
        dayMidnight.setHours(0, 0, 0, 0);
        const dayIndex = Math.round(
          (dayMidnight.getTime() - weekStart.getTime()) / (24 * 60 * 60 * 1000),
        );
        if (dayIndex < 0 || dayIndex >= dayCount) continue;

        const startMin = (startDate.getHours() - startHour) * 60 + startDate.getMinutes();
        const durationMin = Math.max(15, Math.round(dur / 60000));

        placed.push({
          ev,
          occStart,
          occEnd,
          dayIndex,
          topMin: startMin,
          heightMin: durationMin,
          color: calendarStore.colorFor(ev.calendar_id),
          col: 0,
          cols: 1,
        });
      }
    }
    assignColumns(placed);
    return placed;
  }

  /// Distribute overlapping events into vertical column slots, Google-Calendar
  /// style. Same algorithm: sweep events sorted by start, place each in the
  /// first column whose last event ended at-or-before this one's start;
  /// otherwise allocate a new column. Events that share an overlap cluster
  /// (transitive overlap) all get `cols` = the cluster's column count, so a
  /// 3-way overlap renders as three equal-width slots even if only two events
  /// pairwise overlap at any single instant.
  function assignColumns(events: PlacedEvent[]) {
    const byDay = new Map<number, PlacedEvent[]>();
    for (const p of events) {
      const arr = byDay.get(p.dayIndex) ?? [];
      arr.push(p);
      byDay.set(p.dayIndex, arr);
    }
    for (const dayEvents of byDay.values()) {
      dayEvents.sort((a, b) => a.occStart - b.occStart || a.occEnd - b.occEnd);

      // Break the day into clusters of transitive overlap. Within a cluster,
      // every event ends after the next event starts at least once removed.
      let i = 0;
      while (i < dayEvents.length) {
        let clusterEnd = dayEvents[i].occEnd;
        let j = i + 1;
        while (j < dayEvents.length && dayEvents[j].occStart < clusterEnd) {
          clusterEnd = Math.max(clusterEnd, dayEvents[j].occEnd);
          j++;
        }
        // Cluster is dayEvents[i..j). Assign columns.
        const cluster = dayEvents.slice(i, j);
        const colEnds: number[] = []; // index → end time of last event in that column
        for (const ev of cluster) {
          let placed = false;
          for (let c = 0; c < colEnds.length; c++) {
            if (ev.occStart >= colEnds[c]) {
              ev.col = c;
              colEnds[c] = ev.occEnd;
              placed = true;
              break;
            }
          }
          if (!placed) {
            ev.col = colEnds.length;
            colEnds.push(ev.occEnd);
          }
        }
        const cols = colEnds.length;
        for (const ev of cluster) ev.cols = cols;
        i = j;
      }
    }
  }

  const placedEvents = $derived(placeEvents());

  function eventsForDay(idx: number): PlacedEvent[] {
    return placedEvents.filter((p) => p.dayIndex === idx);
  }

  function fmtTimeRange(p: PlacedEvent): string {
    const fmt = (d: Date) =>
      `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
    return `${fmt(new Date(p.occStart))}–${fmt(new Date(p.occEnd))}`;
  }

  /// Meeting (≥2 attendees) where the user is on the invite list but hasn't
  /// answered yet. Renders with a lighter tone so it's visually distinct
  /// from confirmed events the user has explicitly accepted — at a glance
  /// "needs RSVP". Events the user isn't invited to (shared calendars,
  /// other people's blocks) render normally.
  function isUnansweredMeeting(ev: DesktopCalendarEvent): boolean {
    const atts = ev.attendees ?? [];
    if (atts.length < 2) return false;
    const acctEmail = accountStore.activeAccount?.email?.toLowerCase() ?? "";
    let mine: typeof atts[number] | null = null;
    for (const a of atts) {
      const lower = a.email.toLowerCase();
      if (lower === acctEmail || identityStore.findByEmail(a.email)) {
        mine = a;
        break;
      }
    }
    if (!mine) return false;
    const ps = (mine.partstat ?? "").toUpperCase();
    return ps === "" || ps === "NEEDS-ACTION";
  }

  // ── Sidebar collapse ──

  const SIDEBAR_KEY = "ddmail_calendar_sidebar_open";
  function loadSidebarOpen(): boolean {
    const raw = localStorage.getItem(SIDEBAR_KEY);
    return raw === null ? true : raw === "1";
  }
  let sidebarOpen = $state(loadSidebarOpen());
  function toggleSidebar() {
    sidebarOpen = !sidebarOpen;
    try { localStorage.setItem(SIDEBAR_KEY, sidebarOpen ? "1" : "0"); } catch {}
  }

  // ── Event detail card ──

  let openedEvent = $state<PlacedEvent | null>(null);
  function openEvent(p: PlacedEvent) {
    openedEvent = p;
  }
  function closeEvent() {
    openedEvent = null;
  }

  /// Open the detail card from a (event_id, occStart) pair coming from a
  /// reminder toast. Synthesises just enough PlacedEvent shape for
  /// EventDetail — we don't need topPx/cols/etc. because the modal is
  /// freestanding, not laid out on the grid.
  function openEventByIdOccurrence(eventId: number, occStartMs: number) {
    const ev = calendarStore.events.find((e) => e.id === eventId);
    if (!ev) {
      console.warn("[reminders] open-event for unknown id", eventId);
      return;
    }
    const dur = (ev.dtend ?? ev.dtstart + 60 * 60 * 1000) - ev.dtstart;
    openedEvent = {
      ev,
      occStart: occStartMs,
      occEnd: occStartMs + dur,
      dayIndex: 0,
      topMin: 0,
      heightMin: 0,
      color: calendarStore.colorFor(ev.calendar_id),
      col: 0,
      cols: 1,
    };
    // Pull the calendar window forward; if it's already focused this is
    // a no-op.
    getCurrentWebviewWindow().setFocus().catch(() => {});
  }

  // ── Reminder scheduling ──
  //
  // After every calendar refresh we feed the Rust scheduler a list of
  // upcoming occurrences. The scheduler INSERT-OR-IGNOREs each entry,
  // so we can over-send freely — snoozed or acked rows are preserved.
  //
  // v1 uses a single global lead-time. Per-event override and VALARM
  // pass-through are next.
  const REMINDER_LEAD_MIN = 15;
  // Look-ahead horizon. Has to be long enough to schedule "tomorrow
  // 09:00 — remind 15 min before" tonight, even if the calendar window
  // gets closed in the meantime. 36 h covers an overnight pause without
  // re-opening the window.
  const REMINDER_HORIZON_MS = 36 * 60 * 60 * 1000;

  function pushRemindersToScheduler() {
    const now = Date.now();
    const horizon = now + REMINDER_HORIZON_MS;
    const reminders: Array<{
      event_id: number;
      occurrence_start_ms: number;
      fire_at_ms: number;
      lead_min: number;
      summary: string;
    }> = [];
    for (const ev of calendarStore.events) {
      if (ev.all_day) continue; // all-day events skipped until we add per-day lead semantics
      if (ev.status === "CANCELLED") continue;
      const occs = ev.rrule ? expandRRule(ev, now, horizon) : [ev.dtstart];
      for (const occ of occs) {
        if (occ < now) continue;       // already happened
        if (occ > horizon) continue;   // outside look-ahead
        const fireAt = occ - REMINDER_LEAD_MIN * 60_000;
        if (fireAt < now - 60_000) continue; // already past — don't backfill
        reminders.push({
          event_id: ev.id,
          occurrence_start_ms: occ,
          fire_at_ms: fireAt,
          lead_min: REMINDER_LEAD_MIN,
          summary: ev.summary || "",
        });
      }
    }
    if (reminders.length === 0) return;
    invoke("schedule_reminders", { reminders }).catch((e) => {
      console.warn("[reminders] schedule failed:", e);
    });
  }

  // Re-push reminders whenever the events list changes. The scheduler's
  // INSERT-OR-IGNORE policy means this is idempotent.
  $effect(() => {
    if (calendarStore.events.length === 0) return;
    pushRemindersToScheduler();
  });

  // Receive open-event from a reminder toast (or any other source) and
  // open the matching event card.
  let unlistenOpenEvent: UnlistenFn | null = null;
  // Cold-start path: when the main window opens the calendar in
  // response to a reminder, it passes ?open=<event_id>:<occ_ms> so this
  // window jumps straight to the event after the calendar list and
  // events have loaded. Without the deferred-open buffer the URL would
  // be parsed before calendarStore.events arrives.
  let pendingOpen: { eventId: number; occStart: number } | null = (() => {
    const raw = new URLSearchParams(window.location.search).get("open");
    if (!raw) return null;
    const [idStr, occStr] = raw.split(":");
    const eventId = parseInt(idStr, 10);
    const occStart = parseInt(occStr, 10);
    return Number.isFinite(eventId) && Number.isFinite(occStart) ? { eventId, occStart } : null;
  })();

  onMount(async () => {
    try {
      unlistenOpenEvent = await listen<{ event_id: number; occurrence_start_ms: number }>(
        "open-event",
        (e) => openEventByIdOccurrence(e.payload.event_id, e.payload.occurrence_start_ms),
      );
    } catch (e) {
      console.warn("[reminders] could not subscribe to open-event:", e);
    }
  });

  $effect(() => {
    if (!pendingOpen) return;
    if (calendarStore.events.length === 0) return;
    const target = pendingOpen;
    pendingOpen = null;
    openEventByIdOccurrence(target.eventId, target.occStart);
  });

  // ── Create-event dialog (double-click on empty grid) ──
  //
  // We capture the (day, time-of-day) of the click and seed EventEdit in
  // create-mode with a 1-hour default duration snapped to 15 min.
  let createDraft = $state<{ dtstart: number; dtend: number } | null>(null);

  function dayColDblClick(dayIdx: number, ev: MouseEvent) {
    // Ignore double-clicks that bubble up from an existing .event chip —
    // those have their own handler that opens detail.
    if ((ev.target as HTMLElement).closest(".event")) return;
    const col = (ev.currentTarget as HTMLElement).getBoundingClientRect();
    const yPx = ev.clientY - col.top;
    const yMin = pxPerMin > 0 ? yPx / pxPerMin : 0;
    const startMin = Math.max(0, Math.floor(yMin / 15) * 15); // 15-min snap
    const dt = new Date(days[dayIdx]);
    dt.setHours(startHour + Math.floor(startMin / 60), startMin % 60, 0, 0);
    const dtstart = dt.getTime();
    createDraft = { dtstart, dtend: dtstart + 60 * 60 * 1000 };
  }

  function closeCreate() {
    createDraft = null;
  }

  async function onCreated() {
    createDraft = null;
    const account = accountStore.activeAccount;
    if (account) await calendarStore.refreshAfterRSVP(account);
  }

  // ── Drag-to-move events ──
  //
  // Only writable calendars get the grabby cursor. We start tracking on
  // pointerdown over an `.event`, record the original Y, and on each
  // pointermove translate the chip visually via a dragOffset map. On
  // pointerup, if the cumulative offset exceeds a small threshold, snap to
  // 15-min and PATCH the event. For recurring series we apply scope="all"
  // (i.e. shift the whole series) — single-instance edits aren't supported
  // server-side yet.
  type DragState = {
    placedKey: string;        // ev.id + ":" + occStart
    ev: DesktopCalendarEvent;
    occStart: number;
    startY: number;
    deltaPx: number;          // current minutes offset (1 px = 1 min)
  };
  let drag = $state<DragState | null>(null);
  // Per-placed-event Y offset for the visual translation while dragging.
  // `dy` is in pixels (already snapped to 15-min steps) so the chip jumps
  // by exact quarter-hour amounts even when each minute is < 1 px wide.
  const dragOffset = $derived(
    drag ? { key: drag.placedKey, dy: snapMinutes(drag.deltaPx) * pxPerMin } : null,
  );

  /// Convert a pixel delta into a minute delta snapped to a 15-minute grid.
  /// Used by both the visual offset and the eventual PATCH payload, so the
  /// preview and the persisted change always agree.
  function snapMinutes(px: number): number {
    if (pxPerMin <= 0) return 0;
    return Math.round(px / pxPerMin / 15) * 15;
  }

  function eventPointerDown(p: PlacedEvent, ev: PointerEvent) {
    const cal = calendarStore.calendars.find(c => c.id === p.ev.calendar_id);
    if (!cal?.can_write) return; // hands-off on read-only calendars
    // Skip if the click is on an interactive child (none today but defensive).
    if ((ev.target as HTMLElement).tagName === "BUTTON") return;
    ev.preventDefault();
    drag = {
      placedKey: p.ev.id + ":" + p.occStart,
      ev: p.ev,
      occStart: p.occStart,
      startY: ev.clientY,
      deltaPx: 0,
    };
    (ev.currentTarget as HTMLElement).setPointerCapture(ev.pointerId);
  }

  function eventPointerMove(ev: PointerEvent) {
    if (!drag) return;
    drag.deltaPx = ev.clientY - drag.startY;
  }

  async function eventPointerUp(p: PlacedEvent, ev: PointerEvent) {
    if (!drag || drag.placedKey !== p.ev.id + ":" + p.occStart) return;
    const minutes = snapMinutes(drag.deltaPx);
    const dragged = drag;
    drag = null;
    try { (ev.currentTarget as HTMLElement).releasePointerCapture(ev.pointerId); } catch {}
    if (Math.abs(minutes) < 15) return; // treat tiny moves as click — no patch
    const account = accountStore.activeAccount;
    if (!account) return;
    const dt = dragged.ev.dtstart + minutes * 60 * 1000;
    const newEnd = dragged.ev.dtend ? dragged.ev.dtend + minutes * 60 * 1000 : null;
    try {
      const body: Record<string, unknown> = {
        scope: "all",
        dtstart: dt,
      };
      if (newEnd != null) body.dtend = newEnd;
      await invoke("v2_patch_event", {
        accountId: account.id,
        eventId: dragged.ev.id,
        body,
      });
      await calendarStore.refreshAfterRSVP(account);
    } catch (e) {
      console.error("[calendar] drag patch failed:", e);
      alert(`Не удалось перенести событие: ${e}`);
    }
  }

  // ── Resize-by-edge ──
  //
  // Two grab strips at the top and bottom of each writable event let the
  // user change start time (top edge) or end time (bottom edge) without
  // touching the move-drag flow. State lives separately from `drag` so
  // a press on the handle never starts a chip-move and vice versa.
  //
  // Visual: while resizing we mutate the rendered `top` / `height` —
  // NOT `transform` like move-drag does — so the chip really shrinks/
  // grows in place. Pixels snap to 15 min steps via `snapMinutes`. We
  // clamp so the duration never falls below 15 min and the top never
  // crosses the bottom (or the day's start boundary).
  type ResizeState = {
    placedKey: string;
    ev: DesktopCalendarEvent;
    occStart: number;
    edge: "top" | "bottom";
    /** Original `topMin` / `heightMin` captured at pointerdown so the
     *  pointermove math always references a stable baseline (PlacedEvent
     *  rows can re-shuffle if calendarStore.events refreshes mid-drag). */
    baseTopMin: number;
    baseHeightMin: number;
    startY: number;
    deltaPx: number;
  };
  let resize = $state<ResizeState | null>(null);

  /// Clamped minute delta for the current resize. Returns the SNAPPED
  /// value, already constrained to "min 15-min duration" and "stay inside
  /// the visible-hour band". The same value drives both the live preview
  /// and the eventual PATCH so visuals never lie.
  function resizeMinutes(r: ResizeState): number {
    const m = snapMinutes(r.deltaPx);
    if (r.edge === "top") {
      // Can't shrink duration below 15 min, can't push top above startHour=0.
      const min = -r.baseTopMin;
      const max = r.baseHeightMin - 15;
      return Math.max(min, Math.min(max, m));
    }
    // bottom edge: can't shrink below 15 min duration.
    const min = -(r.baseHeightMin - 15);
    return Math.max(min, m);
  }

  function edgePointerDown(p: PlacedEvent, edge: "top" | "bottom", ev: PointerEvent) {
    const cal = calendarStore.calendars.find(c => c.id === p.ev.calendar_id);
    if (!cal?.can_write) return;
    ev.preventDefault();
    ev.stopPropagation(); // don't bleed into the move-drag pointerdown
    resize = {
      placedKey: p.ev.id + ":" + p.occStart,
      ev: p.ev,
      occStart: p.occStart,
      edge,
      baseTopMin: p.topMin,
      baseHeightMin: p.heightMin,
      startY: ev.clientY,
      deltaPx: 0,
    };
    (ev.currentTarget as HTMLElement).setPointerCapture(ev.pointerId);
  }

  function edgePointerMove(ev: PointerEvent) {
    if (!resize) return;
    resize.deltaPx = ev.clientY - resize.startY;
  }

  async function edgePointerUp(p: PlacedEvent, ev: PointerEvent) {
    if (!resize || resize.placedKey !== p.ev.id + ":" + p.occStart) return;
    const minutes = resizeMinutes(resize);
    const r = resize;
    resize = null;
    try { (ev.currentTarget as HTMLElement).releasePointerCapture(ev.pointerId); } catch {}
    if (minutes === 0) return;
    const account = accountStore.activeAccount;
    if (!account) return;
    const body: Record<string, unknown> = { scope: "all" };
    if (r.edge === "top") {
      // Top edge moves dtstart by `minutes` (signed); dtend stays put.
      body.dtstart = r.ev.dtstart + minutes * 60_000;
    } else {
      // Bottom edge moves dtend by `minutes`. Events without an explicit
      // dtend imply a 1-hour duration — surface that to the server.
      const oldEnd = r.ev.dtend ?? r.ev.dtstart + 60 * 60_000;
      body.dtend = oldEnd + minutes * 60_000;
    }
    try {
      await invoke("v2_patch_event", {
        accountId: account.id,
        eventId: r.ev.id,
        body,
      });
      await calendarStore.refreshAfterRSVP(account);
    } catch (e) {
      console.error("[calendar] resize patch failed:", e);
      alert(`Не удалось изменить длительность: ${e}`);
    }
  }

  // ── Color picker popover state ──

  let pickerFor = $state<number | null>(null);
  function openPicker(id: number, e: MouseEvent) {
    e.stopPropagation();
    pickerFor = pickerFor === id ? null : id;
  }
  function closePicker() {
    pickerFor = null;
  }

  function onGlobalKey(e: KeyboardEvent) {
    if (e.key !== "Escape") return;
    if (pickerFor !== null) { pickerFor = null; return; }
    if (openedEvent !== null) { openedEvent = null; return; }
    if (createDraft !== null) { createDraft = null; return; }
    void invoke("close_window", { label: "calendar" });
  }
  function pickColor(id: number, color: string) {
    calendarStore.setColor(id, color);
    pickerFor = null;
  }
</script>

<svelte:window onclick={closePicker} onkeydown={onGlobalKey} />


<div class="cal" style:--day-count={dayCount}>
  <header class="topbar">
    <button
      class="btn-toggle"
      onclick={toggleSidebar}
      title={sidebarOpen ? "Скрыть панель календарей" : "Показать панель календарей"}
      aria-label="Toggle calendar panel"
      aria-pressed={sidebarOpen}
    >
      <!-- Chevron points toward where the panel will go: left when open
           (collapse to the left), right when collapsed (expand back out). -->
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
        {#if sidebarOpen}
          <polyline points="15 18 9 12 15 6"/>
        {:else}
          <polyline points="9 18 15 12 9 6"/>
        {/if}
      </svg>
    </button>
    <div class="nav">
      <button class="btn-nav" onclick={prevWeek} title="Предыдущая неделя" aria-label="Предыдущая неделя">‹</button>
      <button class="btn-today" onclick={thisWeek}>Сегодня</button>
      <button class="btn-nav" onclick={nextWeek} title="Следующая неделя" aria-label="Следующая неделя">›</button>
    </div>
    <h1 class="title">{fmtMonthYear(weekStart)}</h1>

    <div class="view-toggles">
      <button
        class="btn-toggle-pill"
        class:on={!workdaysOnly}
        onclick={toggleWorkdays}
        title="5 рабочих / вся неделя"
      >
        {workdaysOnly ? "5 дней" : "7 дней"}
      </button>
      <button
        class="btn-toggle-pill"
        class:on={showNonWorkHours}
        onclick={toggleNonWorkHours}
        title="Показывать нерабочее время"
      >
        {showNonWorkHours ? "0–24" : "8–18"}
      </button>
    </div>
  </header>

  <div class="layout" class:sidebar-collapsed={!sidebarOpen}>
    <aside class="cal-list">
      <div class="cal-list-title">Календари</div>
      {#if calendarStore.loading && calendarStore.calendars.length === 0}
        <div class="hint">Загрузка…</div>
      {:else if initError}
        <div class="hint err">{initError}</div>
      {:else if calendarStore.error}
        <div class="hint err">{calendarStore.error}</div>
      {:else if calendarStore.calendars.length === 0}
        <div class="hint">Календарей нет. Добавьте источник в веб-интерфейсе сервера.</div>
      {:else}
        {#each calendarStore.calendars as c (c.id)}
          {@const visible = calendarStore.isVisible(c.id)}
          {@const color = calendarStore.colorFor(c.id)}
          <label class="cal-row" class:dim={!visible}>
            <input
              type="checkbox"
              checked={visible}
              onchange={() => {
                const account = accountStore.activeAccount;
                if (account) calendarStore.toggleVisibility(c.id, account);
              }}
            />
            <button
              class="swatch"
              style:background={color}
              title="Сменить цвет"
              aria-label="Сменить цвет"
              onclick={(e) => openPicker(c.id, e)}
            ></button>
            <span class="name" title={c.description || c.name}>{c.name}</span>
          </label>
          {#if pickerFor === c.id}
            <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
            <div class="picker" onclick={(e) => e.stopPropagation()}>
              {#each PALETTE as p}
                <button
                  class="swatch swatch-pick"
                  class:current={color === p}
                  style:background={p}
                  onclick={() => pickColor(c.id, p)}
                  aria-label={p}
                ></button>
              {/each}
              <button class="reset" onclick={() => { calendarStore.resetColor(c.id); pickerFor = null; }}>сброс</button>
            </div>
          {/if}
        {/each}
      {/if}
    </aside>

    <div class="grid-wrap">
      <div class="day-row">
        <div class="time-corner"></div>
        {#each days as d}
          <div class="day-name" class:today={isToday(d)}>{fmtDay(d)}</div>
        {/each}
      </div>

      <div class="body" bind:clientHeight={bodyHeightPx}>
        <div class="time-col" style:height="{totalHeightPx}px">
          {#each hours as h}
            <div class="hour-label" style:height="{hourHeightPx}px">
              {String(h).padStart(2, "0")}:00
            </div>
          {/each}
        </div>
        {#each days as d, di}
          <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
          <div
            class="day-col"
            class:today={isToday(d)}
            style:height="{totalHeightPx}px"
            ondblclick={(e) => dayColDblClick(di, e)}
          >
            {#each hours as _h}
              <div class="hour-cell" style:height="{hourHeightPx}px">
                <div class="quarter q15"></div>
                <div class="quarter q30"></div>
                <div class="quarter q45"></div>
                <div class="quarter q60"></div>
              </div>
            {/each}
            {#each eventsForDay(di) as p (p.ev.id + ":" + p.occStart)}
              {@const writable = (calendarStore.calendars.find(c => c.id === p.ev.calendar_id)?.can_write) ?? false}
              {@const placedKey = p.ev.id + ":" + p.occStart}
              {@const dy = dragOffset && dragOffset.key === placedKey ? dragOffset.dy : 0}
              {@const isResizing = !!resize && resize.placedKey === placedKey}
              {@const rMin = isResizing ? resizeMinutes(resize!) : 0}
              {@const topMinAdj = isResizing && resize!.edge === "top" ? p.topMin + rMin : p.topMin}
              {@const heightMinAdj = isResizing
                ? (resize!.edge === "top" ? p.heightMin - rMin : p.heightMin + rMin)
                : p.heightMin}
              <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
              <div
                class="event"
                class:draggable={writable}
                class:dragging={!!drag && drag.placedKey === placedKey}
                class:resizing={isResizing}
                class:tentative-look={isUnansweredMeeting(p.ev)}
                style:top="{topMinAdj * pxPerMin + dy}px"
                style:height="{heightMinAdj * pxPerMin}px"
                style:left="calc({p.col / p.cols} * (100% - var(--ev-gutter)) + 2px)"
                style:width="calc({1 / p.cols} * (100% - var(--ev-gutter)) - 4px)"
                style:--ev-bg={p.color}
                title="{fmtTimeRange(p)} — {p.ev.summary || '(без названия)'}"
                ondblclick={(e) => { e.stopPropagation(); openEvent(p); }}
                onpointerdown={(e) => eventPointerDown(p, e)}
                onpointermove={eventPointerMove}
                onpointerup={(e) => eventPointerUp(p, e)}
                onpointercancel={() => { drag = null; }}
              >
                {#if writable}
                  <!-- svelte-ignore a11y_no_static_element_interactions -->
                  <div
                    class="resize-handle resize-top"
                    onpointerdown={(e) => edgePointerDown(p, "top", e)}
                    onpointermove={edgePointerMove}
                    onpointerup={(e) => edgePointerUp(p, e)}
                    onpointercancel={() => { resize = null; }}
                    aria-hidden="true"
                  ></div>
                {/if}
                {#if (p.ev.attendees?.length ?? 0) >= 2}
                  <span class="ev-count" title="{p.ev.attendees!.length} участников">{p.ev.attendees!.length}</span>
                {/if}
                <div class="ev-title">{p.ev.summary || "(без названия)"}</div>
                {#if writable}
                  <!-- svelte-ignore a11y_no_static_element_interactions -->
                  <div
                    class="resize-handle resize-bottom"
                    onpointerdown={(e) => edgePointerDown(p, "bottom", e)}
                    onpointermove={edgePointerMove}
                    onpointerup={(e) => edgePointerUp(p, e)}
                    onpointercancel={() => { resize = null; }}
                    aria-hidden="true"
                  ></div>
                {/if}
              </div>
            {/each}
            {#if isToday(d) && nowOffsetPx !== null}
              <div class="now-line" style:top="{nowOffsetPx}px" aria-hidden="true"></div>
            {/if}
          </div>
        {/each}
      </div>
    </div>
  </div>

  {#if openedEvent}
    <EventDetail
      event={openedEvent.ev}
      occStart={openedEvent.occStart}
      occEnd={openedEvent.occEnd}
      onclose={closeEvent}
    />
  {/if}

  {#if createDraft}
    <EventEdit
      event={null}
      occStart={createDraft.dtstart}
      occEnd={createDraft.dtend}
      onclose={closeCreate}
      onsaved={onCreated}
    />
  {/if}
</div>

<style>
  .cal {
    display: flex;
    flex-direction: column;
    height: 100vh;
    background: var(--bg-primary);
    color: var(--text-primary);
    font-family: var(--font-family);
    font-size: var(--font-size);
  }

  .topbar {
    display: flex;
    align-items: center;
    gap: 16px;
    padding: 8px 16px;
    border-bottom: 1px solid var(--border-color);
    height: var(--header-height);
    flex-shrink: 0;
  }
  .nav { display: flex; align-items: center; gap: 4px; }
  .btn-nav, .btn-today {
    border: 1px solid var(--border-color);
    background: var(--bg-primary);
    color: var(--text-primary);
    border-radius: 8px;
    cursor: pointer;
    font-family: inherit;
    font-size: inherit;
  }
  .btn-nav { width: 32px; height: 32px; padding: 0; font-size: 18px; line-height: 1; }
  .btn-today { padding: 6px 14px; }
  .btn-nav:hover, .btn-today:hover { background: var(--bg-hover); }
  .title { font-size: 16px; font-weight: 600; margin: 0; text-transform: capitalize; }

  .btn-toggle {
    width: 36px; height: 36px;
    display: flex; align-items: center; justify-content: center;
    border: none; background: none;
    border-radius: 8px;
    color: var(--text-secondary);
    cursor: pointer;
    flex-shrink: 0;
  }
  .btn-toggle:hover { background: var(--bg-hover); color: var(--text-primary); }

  .view-toggles {
    margin-left: auto;
    display: flex;
    gap: 6px;
  }
  .btn-toggle-pill {
    padding: 6px 12px;
    border: 1px solid var(--border-color);
    border-radius: 14px;
    background: var(--bg-primary);
    color: var(--text-secondary);
    font-family: inherit;
    font-size: var(--font-size-sm);
    cursor: pointer;
    transition: background var(--transition), color var(--transition), border-color var(--transition);
  }
  .btn-toggle-pill:hover { background: var(--bg-hover); color: var(--text-primary); }
  .btn-toggle-pill.on {
    background: var(--text-accent);
    border-color: var(--text-accent);
    color: var(--text-on-active);
  }

  .layout {
    flex: 1;
    display: flex;
    min-height: 0;
  }

  /* ── Calendar list sidebar ── */
  .cal-list {
    width: 240px;
    flex-shrink: 0;
    border-right: 1px solid var(--border-color);
    overflow-y: auto;
    padding: 12px 8px;
    background: var(--bg-primary);
    transition: width 150ms ease, padding 150ms ease, border-color 150ms ease;
  }
  .layout.sidebar-collapsed .cal-list {
    width: 0;
    padding: 12px 0;
    border-right-color: transparent;
    overflow: hidden;
  }
  .cal-list-title {
    font-size: var(--font-size-xs);
    text-transform: uppercase;
    color: var(--text-secondary);
    padding: 0 8px 8px;
    letter-spacing: 0.04em;
  }
  .hint {
    padding: 8px 12px;
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
  }
  .hint.err { color: #e8616a; }

  .cal-row {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 6px 8px;
    border-radius: 6px;
    cursor: pointer;
  }
  .cal-row:hover { background: var(--bg-hover); }
  .cal-row.dim .name { color: var(--text-secondary); }
  .cal-row input[type="checkbox"] { cursor: pointer; }
  .swatch {
    width: 14px;
    height: 14px;
    border-radius: 4px;
    border: 1px solid rgba(0,0,0,0.1);
    flex-shrink: 0;
    cursor: pointer;
    padding: 0;
  }
  .name {
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: var(--font-size-sm);
  }
  .picker {
    margin: 4px 8px 8px 32px;
    padding: 6px;
    border: 1px solid var(--border-color);
    border-radius: 8px;
    background: var(--bg-primary);
    box-shadow: var(--shadow-md);
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    align-items: center;
  }
  .swatch-pick {
    width: 18px; height: 18px;
    border: 2px solid transparent;
  }
  .swatch-pick.current { border-color: var(--text-primary); }
  .reset {
    margin-left: auto;
    background: none;
    border: none;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    cursor: pointer;
    padding: 2px 4px;
  }
  .reset:hover { color: var(--text-primary); }

  /* ── Grid ── */
  .grid-wrap {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-width: 0;
  }
  .day-row {
    display: grid;
    grid-template-columns: 60px repeat(var(--day-count, 5), 1fr);
    border-bottom: 1px solid var(--border-color);
    background: var(--bg-primary);
    flex-shrink: 0;
  }
  .time-corner { border-right: 1px solid var(--border-color); }
  .day-name {
    padding: 12px 8px;
    text-align: center;
    border-right: 1px solid var(--border-color);
    font-weight: 500;
    color: var(--text-secondary);
    text-transform: capitalize;
  }
  .day-name:last-child { border-right: none; }
  .day-name.today { color: var(--text-accent); font-weight: 700; }

  .body {
    display: grid;
    grid-template-columns: 60px repeat(var(--day-count, 5), 1fr);
    overflow-y: auto;
    flex: 1;
    min-height: 0;
  }
  .time-col {
    border-right: 1px solid var(--border-color);
    background: var(--bg-primary);
    position: sticky;
    left: 0;
    z-index: 1;
  }
  .hour-label {
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
    text-align: right;
    padding-right: 8px;
    padding-top: 2px;
    box-sizing: border-box;
  }
  .day-col {
    border-right: 1px solid var(--border-color);
    position: relative;
  }
  .day-col:last-child { border-right: none; }
  .day-col.today { background: color-mix(in srgb, var(--text-accent) 5%, transparent); }
  .hour-cell {
    border-bottom: 1px solid var(--border-color);
    display: flex;
    flex-direction: column;
    box-sizing: border-box;
  }
  .quarter { flex: 1; box-sizing: border-box; }
  .q15, .q45 { border-bottom: 1px dotted var(--border-color); }
  .q30 { border-bottom: 1px dashed var(--border-color); }

  /* ── Events ──
     left/width are set inline based on col/cols so overlapping events split
     the day column horizontally instead of stacking opaquely on top of each
     other. The right-side gutter (--ev-gutter) reserves a strip of empty
     column space no matter how many events overlap, so the user can always
     double-click into a busy column to create a new event without having to
     pick a free vertical lane manually. */
  .day-col { --ev-gutter: 8px; }
  .event {
    position: absolute;
    border-radius: 4px;
    padding: 2px 6px;
    color: #fff;
    font-size: var(--font-size-xs);
    box-shadow: 0 1px 2px rgba(0,0,0,0.15);
    overflow: hidden;
    cursor: pointer;
    border: 1px solid rgba(255,255,255,0.15);
    box-sizing: border-box;
    touch-action: none;
    background: var(--ev-bg);
  }
  /* Meeting awaiting my RSVP — lighten by mixing in the page background.
     `color-mix` keeps the hue but pulls saturation/lightness toward neutral,
     so a saturated blue meeting becomes a softer blue without losing its
     calendar identity. */
  .event.tentative-look {
    background: color-mix(in srgb, var(--ev-bg) 70%, var(--bg-primary));
    border-style: dashed;
    border-color: color-mix(in srgb, var(--ev-bg) 60%, transparent);
    color: color-mix(in srgb, #fff 85%, transparent);
  }
  .event.draggable { cursor: grab; }
  .event.dragging {
    cursor: grabbing;
    z-index: 2;
    opacity: 0.85;
    box-shadow: 0 4px 12px rgba(0,0,0,0.35);
    transition: none;
  }
  .event.resizing {
    z-index: 2;
    box-shadow: 0 4px 12px rgba(0,0,0,0.35);
    transition: none;
  }
  /* Grab strips for edge-resize. Thin transparent zones on the top and
     bottom of the event chip — hovering reveals a subtle highlight so the
     interaction is discoverable. `touch-action: none` keeps the pointer
     stream consistent with the rest of the chip. */
  .resize-handle {
    position: absolute;
    left: 0;
    right: 0;
    height: 6px;
    cursor: ns-resize;
    touch-action: none;
    z-index: 1;
  }
  .resize-top { top: 0; }
  .resize-bottom { bottom: 0; }
  .event:hover .resize-handle {
    background: rgba(255, 255, 255, 0.25);
  }
  .ev-title {
    line-height: 1.2;
    /* Wrap to as many lines as fit; the `.event` parent has
       overflow:hidden so anything past the chip just gets clipped. No
       ellipsis — the full title is one tooltip away. */
    overflow-wrap: anywhere;
    word-break: break-word;
  }
  /* Attendee count badge in the upper-right of the event tile. Only renders
     when the event has 2+ attendees (a meeting, not a personal block). */
  .ev-count {
    position: absolute;
    top: 2px;
    right: 4px;
    min-width: 16px;
    height: 16px;
    padding: 0 5px;
    border-radius: 8px;
    background: rgba(255, 255, 255, 0.28);
    color: #fff;
    font-size: 10px;
    font-weight: 700;
    line-height: 16px;
    text-align: center;
    pointer-events: none;
  }

  /* Current-time marker. Sits above events (z-index > .event.dragging's 2)
     and stretches the full column width with a small bullet on the left so
     the line is readable against any background colour. */
  .now-line {
    position: absolute;
    left: 0;
    right: 0;
    height: 2px;
    background: #e53935;
    z-index: 3;
    pointer-events: none;
  }
  .now-line::before {
    content: "";
    position: absolute;
    left: -3px;
    top: -3px;
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: #e53935;
  }
</style>
