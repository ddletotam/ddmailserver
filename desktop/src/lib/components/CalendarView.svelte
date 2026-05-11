<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import { accountStore } from "../stores/accounts.svelte";
  import { mailStore } from "../stores/mail.svelte";
  import { calendarStore, PALETTE } from "../stores/calendar.svelte";
  import type { DesktopCalendarEvent } from "../types/calendar";

  // Default view: 5 working days (Mon-Fri), 8:00-18:00, 15-minute subdivisions.
  const HOUR_HEIGHT = 60; // px per hour — 1 px per minute, 15 px per quarter
  const startHour = 8;
  const endHour = 18; // bottom edge — last hour cell is 17:00..18:00
  const dayCount = 5;
  const totalHeightPx = (endHour - startHour) * HOUR_HEIGHT;

  const hours = Array.from({ length: endHour - startHour }, (_, i) => startHour + i);

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
    e.setDate(d.getDate() + dayCount); // exclusive end (Sat 00:00 when start = Mon)
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
    dayIndex: number;
    topPx: number;
    heightPx: number;
    color: string;
  }

  function placeEvents(): PlacedEvent[] {
    const placed: PlacedEvent[] = [];
    const minView = weekStart.getTime();
    const maxView = endOfWeek(weekStart).getTime();

    for (const ev of calendarStore.events) {
      if (ev.all_day) continue; // all-day band not implemented yet
      if (ev.dtstart >= maxView) continue;
      const end = ev.dtend ?? ev.dtstart + 60 * 60 * 1000; // default 1h if missing
      if (end <= minView) continue;

      const startDate = new Date(ev.dtstart);
      const dayMidnight = new Date(startDate);
      dayMidnight.setHours(0, 0, 0, 0);
      const dayIndex = Math.round((dayMidnight.getTime() - weekStart.getTime()) / (24 * 60 * 60 * 1000));
      if (dayIndex < 0 || dayIndex >= dayCount) continue;

      const startMin = (startDate.getHours() - startHour) * 60 + startDate.getMinutes();
      const durationMin = Math.max(15, Math.round((end - ev.dtstart) / 60000));
      const top = startMin; // 1 px/min
      const height = durationMin;

      placed.push({
        ev,
        dayIndex,
        topPx: top,
        heightPx: height,
        color: calendarStore.colorFor(ev.calendar_id),
      });
    }
    return placed;
  }

  const placedEvents = $derived(placeEvents());

  function eventsForDay(idx: number): PlacedEvent[] {
    return placedEvents.filter((p) => p.dayIndex === idx);
  }

  function fmtTimeRange(ev: DesktopCalendarEvent): string {
    const s = new Date(ev.dtstart);
    const fmt = (d: Date) =>
      `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
    if (!ev.dtend) return fmt(s);
    return `${fmt(s)}–${fmt(new Date(ev.dtend))}`;
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

<div class="cal">
  <header class="topbar">
    <button
      class="btn-toggle"
      onclick={toggleSidebar}
      title={sidebarOpen ? "Скрыть панель календарей" : "Показать панель календарей"}
      aria-label="Toggle calendar panel"
      aria-pressed={sidebarOpen}
    >
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
        <line x1="4" y1="6" x2="20" y2="6"/>
        <line x1="4" y1="12" x2="20" y2="12"/>
        <line x1="4" y1="18" x2="20" y2="18"/>
      </svg>
    </button>
    <div class="nav">
      <button class="btn-nav" onclick={prevWeek} title="Предыдущая неделя" aria-label="Предыдущая неделя">‹</button>
      <button class="btn-today" onclick={thisWeek}>Сегодня</button>
      <button class="btn-nav" onclick={nextWeek} title="Следующая неделя" aria-label="Следующая неделя">›</button>
    </div>
    <h1 class="title">{fmtMonthYear(weekStart)}</h1>
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
            {#each eventsForDay(di) as p (p.ev.id)}
              <div
                class="event"
                style:top="{p.topPx}px"
                style:height="{p.heightPx}px"
                style:background={p.color}
                title={p.ev.summary}
              >
                <div class="ev-time">{fmtTimeRange(p.ev)}</div>
                <div class="ev-title">{p.ev.summary || "(без названия)"}</div>
              </div>
            {/each}
          </div>
        {/each}
      </div>
    </div>
  </div>
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
    grid-template-columns: 60px repeat(5, 1fr);
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
    grid-template-columns: 60px repeat(5, 1fr);
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

  /* ── Events ── */
  .event {
    position: absolute;
    left: 2px;
    right: 2px;
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
