<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import { RRule } from "rrule";
  import { accountStore } from "../stores/accounts.svelte";
  import { mailStore } from "../stores/mail.svelte";
  import { calendarStore, PALETTE } from "../stores/calendar.svelte";
  import EventDetail from "./EventDetail.svelte";
  import type { DesktopCalendarEvent } from "../types/calendar";

  const HOUR_HEIGHT = 60; // px per hour — 1 px per minute, 15 px per quarter

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
  const totalHeightPx = $derived((endHour - startHour) * HOUR_HEIGHT);
  const hours = $derived(
    Array.from({ length: endHour - startHour }, (_, i) => startHour + i),
  );

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

  onMount(async () => {
    const account = accountStore.activeAccount;
    if (!account) {
      initError = "Нет активной учётки. Сначала войдите в основном окне.";
      return;
    }
    try {
      await mailStore.ensureActivated(account);
      await calendarStore.load(account);
      await calendarStore.startWatching(account);
    } catch (e: unknown) {
      initError = e instanceof Error ? e.message : String(e);
    }
  });

  onDestroy(() => {
    calendarStore.stopWatching();
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
    topPx: number;
    heightPx: number;
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
      return occs.map((d) => d.getTime());
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
        const top = startMin; // 1 px/min
        const height = durationMin;

        placed.push({
          ev,
          occStart,
          occEnd,
          dayIndex,
          topPx: top,
          heightPx: height,
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

  // ── Color picker popover state ──

  let pickerFor = $state<number | null>(null);
  function openPicker(id: number, e: MouseEvent) {
    e.stopPropagation();
    pickerFor = pickerFor === id ? null : id;
  }
  function closePicker() {
    pickerFor = null;
  }
  function pickColor(id: number, color: string) {
    calendarStore.setColor(id, color);
    pickerFor = null;
  }
</script>

<svelte:window onclick={closePicker} />

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

      <div class="body">
        <div class="time-col" style:height="{totalHeightPx}px">
          {#each hours as h}
            <div class="hour-label" style:height="{HOUR_HEIGHT}px">
              {String(h).padStart(2, "0")}:00
            </div>
          {/each}
        </div>
        {#each days as d, di}
          <div class="day-col" class:today={isToday(d)} style:height="{totalHeightPx}px">
            {#each hours as _h}
              <div class="hour-cell" style:height="{HOUR_HEIGHT}px">
                <div class="quarter q15"></div>
                <div class="quarter q30"></div>
                <div class="quarter q45"></div>
                <div class="quarter q60"></div>
              </div>
            {/each}
            {#each eventsForDay(di) as p (p.ev.id + ":" + p.occStart)}
              <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
              <div
                class="event"
                style:top="{p.topPx}px"
                style:height="{p.heightPx}px"
                style:left="calc({(p.col / p.cols) * 100}% + 2px)"
                style:width="calc({(1 / p.cols) * 100}% - 4px)"
                style:background={p.color}
                title={p.ev.summary}
                ondblclick={() => openEvent(p)}
              >
                <div class="ev-time">{fmtTimeRange(p)}</div>
                <div class="ev-title">{p.ev.summary || "(без названия)"}</div>
              </div>
            {/each}
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
     other. */
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
  }
  .ev-time { font-weight: 600; opacity: 0.85; line-height: 1.2; }
  .ev-title { line-height: 1.2; white-space: nowrap; text-overflow: ellipsis; overflow: hidden; }
</style>
