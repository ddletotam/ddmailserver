<script lang="ts">
  import { invoke } from "@tauri-apps/api/core";
  import { accountStore } from "../stores/accounts.svelte";
  import { calendarStore } from "../stores/calendar.svelte";
  import { identityStore } from "../stores/identity.svelte";
  import type { DesktopCalendarEvent, DesktopCalendarAttendee } from "../types/calendar";

  interface Props {
    event: DesktopCalendarEvent;
    occStart: number; // ms — actual instance start (matters for RRULE)
    occEnd: number;
    onclose: () => void;
  }
  let { event, occStart, occEnd, onclose }: Props = $props();

  // Calendar context: name + color, looked up by id so it survives the case
  // where the user reorders or renames the calendar after opening the card.
  const calendar = $derived(
    calendarStore.calendars.find((c) => c.id === event.calendar_id) ?? null,
  );
  const color = $derived(calendarStore.colorFor(event.calendar_id));

  function fmtDateTime(ms: number, allDay: boolean): string {
    const d = new Date(ms);
    if (allDay) {
      return d.toLocaleDateString(undefined, {
        weekday: "long",
        day: "numeric",
        month: "long",
        year: "numeric",
      });
    }
    return d.toLocaleString(undefined, {
      weekday: "short",
      day: "numeric",
      month: "short",
      year: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
  }

  function fmtRange(): string {
    const start = fmtDateTime(occStart, event.all_day);
    if (!event.dtend && !occEnd) return start;
    const end = fmtDateTime(occEnd, event.all_day);
    return `${start} → ${end}`;
  }

  // URL detection. We split text into runs so non-link chunks render plain
  // and links render as buttons that invoke the existing open_url command
  // (which routes to explorer.exe / xdg-open / open per platform).
  // Matches http(s) and bare www. URLs; trailing punctuation gets dropped.
  const URL_RE = /(https?:\/\/[^\s<>()]+|www\.[^\s<>()]+)/gi;
  const TRAILING_PUNCT = /[.,;:!?)\]}'"]+$/;

  interface TextRun {
    kind: "text" | "link";
    value: string;
    href?: string;
  }

  function splitLinks(text: string): TextRun[] {
    const runs: TextRun[] = [];
    let lastIndex = 0;
    for (const m of text.matchAll(URL_RE)) {
      const idx = m.index ?? 0;
      let raw = m[0];
      // Trim trailing punctuation that's almost never part of the URL
      const trailing = raw.match(TRAILING_PUNCT);
      let trail = "";
      if (trailing) {
        trail = trailing[0];
        raw = raw.slice(0, -trail.length);
      }
      if (idx > lastIndex) {
        runs.push({ kind: "text", value: text.slice(lastIndex, idx) });
      }
      runs.push({
        kind: "link",
        value: raw,
        href: raw.startsWith("www.") ? `https://${raw}` : raw,
      });
      if (trail) runs.push({ kind: "text", value: trail });
      lastIndex = idx + raw.length + trail.length;
    }
    if (lastIndex < text.length) {
      runs.push({ kind: "text", value: text.slice(lastIndex) });
    }
    return runs;
  }

  async function openLink(href: string) {
    try {
      await invoke("open_url", { url: href });
    } catch (e) {
      console.error("[event-detail] open_url failed:", e);
    }
  }

  function backdropClick(e: MouseEvent) {
    if (e.target === e.currentTarget) onclose();
  }

  function keyHandler(e: KeyboardEvent) {
    if (e.key === "Escape") onclose();
  }

  // Visible fields: everything non-empty that adds information. UID/etag/
  // ical_data deliberately skipped — they're protocol plumbing, not user
  // content.
  const hasOrganizer = $derived(!!(event.organizer_email || event.organizer_name));
  const organizerLine = $derived(
    event.organizer_name && event.organizer_email
      ? `${event.organizer_name} <${event.organizer_email}>`
      : event.organizer_name || event.organizer_email || "",
  );

  // ── Attendees + RSVP ──

  const attendees = $derived(event.attendees ?? []);

  // Match attendee row against the user's identities (primary + aliases).
  // For RSVP pills to be meaningful we need to know which row IS the user;
  // otherwise we can't tell which PARTSTAT to send back.
  const myAttendee = $derived.by<DesktopCalendarAttendee | null>(() => {
    for (const a of attendees) {
      if (identityStore.findByEmail(a.email)) return a;
    }
    return null;
  });

  // RSVP is only meaningful on calendars we can write back to. ICS sources
  // are read-only — the server will reject anyway, no point showing pills.
  const canRSVP = $derived.by(() => {
    if (!myAttendee) return false;
    if (!calendar) return false;
    return calendar.source_type !== "ics_url" && calendar.source_type !== "ics_import";
  });

  // Local state for optimistic PARTSTAT updates. Falls back to the server
  // value until the user clicks a pill.
  let pendingPartstat = $state<string | null>(null);
  let rsvpError = $state<string | null>(null);
  const currentPartstat = $derived(pendingPartstat ?? myAttendee?.partstat ?? "NEEDS-ACTION");

  async function setRSVP(p: string) {
    const account = accountStore.activeAccount;
    if (!account) return;
    pendingPartstat = p;
    rsvpError = null;
    try {
      await invoke<string>("v2_rsvp_event", {
        accountId: account.id,
        eventId: event.id,
        partstat: p,
      });
      // Refresh calendar view so attendees + chips reflect the new state
      // across all opened cards (rare but harmless to refetch).
      await calendarStore.refreshAfterRSVP(account);
    } catch (e: unknown) {
      pendingPartstat = null;
      rsvpError = e instanceof Error ? e.message : String(e);
      console.error("[event-detail] RSVP failed:", e);
    }
  }

  function partStatBadge(p: string): { label: string; cls: string } {
    switch ((p || "").toUpperCase()) {
      case "ACCEPTED":    return { label: "идёт",       cls: "ps-accepted" };
      case "DECLINED":    return { label: "не идёт",    cls: "ps-declined" };
      case "TENTATIVE":   return { label: "сомнительно", cls: "ps-tentative" };
      case "DELEGATED":   return { label: "делегировал", cls: "ps-other" };
      case "NEEDS-ACTION":
      case "":            return { label: "не ответил",  cls: "ps-pending" };
      default:            return { label: p,             cls: "ps-other" };
    }
  }
</script>

<svelte:window onkeydown={keyHandler} />

<!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
<div class="backdrop" onclick={backdropClick}>
  <div class="card" role="dialog" aria-modal="true" aria-label={event.summary || "Событие"}>
    <header class="card-head" style:border-top-color={color}>
      <div class="head-line">
        <span class="swatch" style:background={color} aria-hidden="true"></span>
        {#if calendar}<span class="cal-name">{calendar.name}</span>{/if}
        <button class="btn-close" onclick={onclose} aria-label="Закрыть">×</button>
      </div>
      <h2 class="summary">{event.summary || "(без названия)"}</h2>
      <div class="when">{fmtRange()}</div>
      {#if event.rrule}<div class="rrule-hint">↻ повторяющееся событие</div>{/if}
    </header>

    <div class="card-body">
      {#if event.location}
        <section class="field">
          <div class="label">Место</div>
          <div class="value">
            {#each splitLinks(event.location) as run}
              {#if run.kind === "link" && run.href}
                <button class="link" onclick={() => openLink(run.href!)}>{run.value}</button>
              {:else}
                <span>{run.value}</span>
              {/if}
            {/each}
          </div>
        </section>
      {/if}

      {#if hasOrganizer}
        <section class="field">
          <div class="label">Организатор</div>
          <div class="value">{organizerLine}</div>
        </section>
      {/if}

      {#if event.status && event.status !== "CONFIRMED"}
        <section class="field">
          <div class="label">Статус</div>
          <div class="value">{event.status}</div>
        </section>
      {/if}

      {#if event.description}
        <section class="field">
          <div class="label">Описание</div>
          <div class="value description">
            {#each splitLinks(event.description) as run}
              {#if run.kind === "link" && run.href}
                <button class="link" onclick={() => openLink(run.href!)}>{run.value}</button>
              {:else}
                <span>{run.value}</span>
              {/if}
            {/each}
          </div>
        </section>
      {/if}

      {#if attendees.length > 0}
        <section class="field">
          <div class="label">Участники ({attendees.length})</div>
          <ul class="attendees">
            {#each attendees as a}
              {@const badge = partStatBadge(a.partstat ?? "")}
              <li class="attendee" class:me={a === myAttendee}>
                <span class="att-name">
                  {a.name || a.email}{#if a.name && a.email}<span class="att-email"> &lt;{a.email}&gt;</span>{/if}
                </span>
                <span class="ps-badge {badge.cls}">{badge.label}</span>
              </li>
            {/each}
          </ul>
        </section>
      {/if}
    </div>

    {#if canRSVP}
      <footer class="rsvp-bar">
        <span class="rsvp-label">Ваш ответ:</span>
        <button
          class="rsvp-pill ps-accepted"
          class:on={currentPartstat === "ACCEPTED"}
          onclick={() => setRSVP("ACCEPTED")}
        >Иду</button>
        <button
          class="rsvp-pill ps-tentative"
          class:on={currentPartstat === "TENTATIVE"}
          onclick={() => setRSVP("TENTATIVE")}
        >Сомнительно</button>
        <button
          class="rsvp-pill ps-declined"
          class:on={currentPartstat === "DECLINED"}
          onclick={() => setRSVP("DECLINED")}
        >Не иду</button>
        {#if rsvpError}<span class="rsvp-err">{rsvpError}</span>{/if}
      </footer>
    {/if}
  </div>
</div>

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    background: rgba(0, 0, 0, 0.4);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 1000;
  }
  .card {
    background: var(--bg-primary);
    color: var(--text-primary);
    border-radius: 12px;
    box-shadow: var(--shadow-md);
    width: min(560px, 90vw);
    max-height: 80vh;
    display: flex;
    flex-direction: column;
    overflow: hidden;
    font-family: var(--font-family);
    font-size: var(--font-size);
  }

  .card-head {
    padding: 16px 20px 12px;
    border-bottom: 1px solid var(--border-color);
    border-top: 4px solid var(--text-accent);
    background: var(--bg-primary);
  }
  .head-line {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 6px;
  }
  .swatch {
    width: 12px;
    height: 12px;
    border-radius: 3px;
    flex-shrink: 0;
  }
  .cal-name {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    flex: 1;
  }
  .btn-close {
    width: 28px;
    height: 28px;
    border: none;
    background: none;
    color: var(--text-secondary);
    font-size: 22px;
    line-height: 1;
    cursor: pointer;
    border-radius: 50%;
  }
  .btn-close:hover {
    background: var(--bg-hover);
    color: var(--text-primary);
  }
  .summary {
    margin: 0 0 6px;
    font-size: 18px;
    font-weight: 600;
    word-break: break-word;
  }
  .when {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    text-transform: capitalize;
  }
  .rrule-hint {
    margin-top: 4px;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }

  .card-body {
    padding: 16px 20px;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    gap: 14px;
  }

  .field .label {
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    margin-bottom: 4px;
  }
  .field .value {
    word-break: break-word;
  }
  .description {
    white-space: pre-wrap;
    line-height: 1.45;
  }

  .link {
    color: var(--text-link);
    background: none;
    border: none;
    padding: 0;
    margin: 0;
    font: inherit;
    cursor: pointer;
    text-decoration: underline;
    word-break: break-all;
  }
  .link:hover {
    color: var(--text-accent);
  }

  /* Attendees */
  .attendees {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .attendee {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 4px 0;
  }
  .attendee.me .att-name { font-weight: 600; }
  .att-name { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .att-email { color: var(--text-secondary); font-weight: normal; }

  .ps-badge {
    font-size: var(--font-size-xs);
    padding: 1px 8px;
    border-radius: 10px;
    border: 1px solid transparent;
    white-space: nowrap;
    flex-shrink: 0;
  }
  .ps-accepted  { background: #1f7a3a22; color: #2e8c4a; border-color: #2e8c4a55; }
  .ps-declined  { background: #c0393922; color: #c03939; border-color: #c0393955; }
  .ps-tentative { background: #c8a83222; color: #c8a832; border-color: #c8a83255; }
  .ps-pending   { background: var(--bg-hover); color: var(--text-secondary); border-color: var(--border-color); }
  .ps-other     { background: var(--bg-hover); color: var(--text-secondary); border-color: var(--border-color); }

  /* RSVP bar */
  .rsvp-bar {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 12px 20px;
    border-top: 1px solid var(--border-color);
    flex-wrap: wrap;
  }
  .rsvp-label { color: var(--text-secondary); font-size: var(--font-size-sm); }
  .rsvp-pill {
    border: 1px solid var(--border-color);
    background: var(--bg-primary);
    color: var(--text-primary);
    padding: 6px 14px;
    border-radius: 16px;
    cursor: pointer;
    font: inherit;
    font-size: var(--font-size-sm);
  }
  .rsvp-pill:hover { background: var(--bg-hover); }
  .rsvp-pill.on.ps-accepted  { background: #2e8c4a; color: #fff; border-color: #2e8c4a; }
  .rsvp-pill.on.ps-tentative { background: #c8a832; color: #fff; border-color: #c8a832; }
  .rsvp-pill.on.ps-declined  { background: #c03939; color: #fff; border-color: #c03939; }
  .rsvp-err {
    margin-left: auto;
    color: #c03939;
    font-size: var(--font-size-xs);
  }
</style>
