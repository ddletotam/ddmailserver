<script lang="ts">
  // Snooze-config webview for the calendar reminder. Loaded with
  // `index.html?view=snooze&event_id=X&occ=Y`. Pulls the row via the
  // `get_reminder` Tauri command (avoids encoding summary in the URL),
  // lets the user pick a snooze delta, and commits via the existing
  // `reminder_action` pipeline — same one the toast buttons use, so the
  // backend state machine stays single-source.
  import { onMount } from "svelte";
  import { invoke } from "@tauri-apps/api/core";
  import { getCurrentWebviewWindow } from "@tauri-apps/api/webviewWindow";

  type Row = {
    event_id: number;
    occurrence_start_ms: number;
    fire_at_ms: number;
    lead_min: number;
    summary: string;
  };

  const params = new URLSearchParams(window.location.search);
  const eventId = Number(params.get("event_id") ?? "0");
  const occMs = Number(params.get("occ") ?? "0");

  let row = $state<Row | null>(null);
  let loadError = $state<string | null>(null);
  let now = $state(Date.now());
  let busy = $state(false);

  let customValue = $state<number>(15);
  let customUnit = $state<"min" | "hour">("min");

  // Quick presets — minutes. The +4ч end is a soft cap; longer than
  // that and the user almost certainly wants the "к началу" option or a
  // calendar reschedule, not a snooze.
  const PRESETS = [5, 15, 30, 60, 240] as const;

  onMount(() => {
    // Async fetch fires off in parallel — Svelte 5's onMount must be
    // synchronous to return a cleanup function, so we can't `await` here
    // directly. The labels and presets render against a `null` row until
    // the load resolves, which is fine because `loadError`/`row` flips
    // their visibility.
    (async () => {
      try {
        const r = await invoke<Row | null>("get_reminder", {
          eventId,
          occurrenceStartMs: occMs,
        });
        if (!r) {
          loadError = "Напоминание не найдено";
          return;
        }
        row = r;
      } catch (e) {
        loadError = String(e);
      }
    })();
    // Keep "in N min until start" labels honest while the window stays open.
    const t = setInterval(() => (now = Date.now()), 30_000);
    return () => clearInterval(t);
  });

  function fmtPreset(min: number): string {
    if (min < 60) return `+${min} мин`;
    const h = min / 60;
    return Number.isInteger(h) ? `+${h} ч` : `+${h.toFixed(1)} ч`;
  }

  function occInFuture(): boolean {
    return row !== null && row.occurrence_start_ms > now;
  }

  function untilStartMin(): number {
    if (!row) return 0;
    return Math.max(0, Math.round((row.occurrence_start_ms - now) / 60_000));
  }

  async function snoozeMinutes(minutes: number) {
    if (busy || !row) return;
    if (minutes <= 0) return;
    busy = true;
    try {
      await invoke("reminder_action", {
        eventId: row.event_id,
        occurrenceStartMs: row.occurrence_start_ms,
        action: `snz:${minutes}`,
      });
      await closeWindow();
    } catch (e) {
      busy = false;
      loadError = `Не удалось отложить: ${e}`;
    }
  }

  async function snoozeAtStart() {
    if (busy || !row) return;
    busy = true;
    try {
      await invoke("reminder_action", {
        eventId: row.event_id,
        occurrenceStartMs: row.occurrence_start_ms,
        action: "snz:atstart",
      });
      await closeWindow();
    } catch (e) {
      busy = false;
      loadError = `Не удалось отложить: ${e}`;
    }
  }

  function submitCustom() {
    const v = Math.floor(customValue);
    if (!Number.isFinite(v) || v <= 0) return;
    const minutes = customUnit === "hour" ? v * 60 : v;
    void snoozeMinutes(minutes);
  }

  async function closeWindow() {
    try {
      await getCurrentWebviewWindow().close();
    } catch {
      // If close fails (rare), hiding still gets us out of the way.
      try { await getCurrentWebviewWindow().hide(); } catch {}
    }
  }

  function onKey(e: KeyboardEvent) {
    if (e.key === "Escape") {
      e.preventDefault();
      void closeWindow();
    } else if (e.key === "Enter") {
      // Enter on the custom field submits it; otherwise no-op so a stray
      // Enter on a preset button (which is the focused element after
      // tab-navigation) doesn't fire twice.
      const active = document.activeElement as HTMLElement | null;
      if (active?.id === "snz-custom") submitCustom();
    }
  }

  function fmtTime(ms: number): string {
    return new Date(ms).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  }
</script>

<svelte:window onkeydown={onKey} />

<div class="root">
  {#if loadError}
    <div class="err">{loadError}</div>
    <div class="footer"><button class="btn ghost" onclick={closeWindow}>Закрыть</button></div>
  {:else if !row}
    <div class="skel">Загрузка…</div>
  {:else}
    <header>
      <h1>Отложить напоминание</h1>
      <p class="summary" title={row.summary}>{row.summary || "Без названия"}</p>
      <p class="when">
        Начало в {fmtTime(row.occurrence_start_ms)}
        {#if occInFuture()}<span class="muted">— через {untilStartMin()} мин</span>{/if}
      </p>
    </header>

    <section class="chips">
      {#each PRESETS as min (min)}
        <button class="chip" disabled={busy} onclick={() => snoozeMinutes(min)}>
          {fmtPreset(min)}
        </button>
      {/each}
    </section>

    <section class="custom">
      <label for="snz-custom" class="lbl">Своё значение</label>
      <div class="row">
        <input
          id="snz-custom"
          type="number"
          min="1"
          step="1"
          bind:value={customValue}
          disabled={busy} />
        <select bind:value={customUnit} disabled={busy}>
          <option value="min">мин</option>
          <option value="hour">ч</option>
        </select>
        <button class="btn primary" disabled={busy || !(customValue > 0)} onclick={submitCustom}>
          Отложить
        </button>
      </div>
    </section>

    {#if occInFuture()}
      <section class="atstart">
        <button class="btn ghost wide" disabled={busy} onclick={snoozeAtStart}>
          Напомнить в момент начала ({fmtTime(row.occurrence_start_ms)})
        </button>
      </section>
    {/if}

    <footer class="footer">
      <button class="btn ghost" disabled={busy} onclick={closeWindow}>Отмена</button>
    </footer>
  {/if}
</div>

<style>
  :global(html, body) {
    margin: 0;
    background: var(--bg-primary, #1e1e1e);
    color: var(--text-primary, #e5e5e5);
    font: 14px/1.4 system-ui, -apple-system, "Segoe UI", sans-serif;
    overflow: hidden;
  }
  .root {
    display: flex;
    flex-direction: column;
    gap: 16px;
    padding: 16px 18px 14px;
    height: 100vh;
    box-sizing: border-box;
  }
  header h1 {
    margin: 0 0 6px;
    font-size: 14px;
    font-weight: 600;
    letter-spacing: 0.02em;
    color: var(--text-secondary, #aaa);
    text-transform: uppercase;
  }
  .summary {
    margin: 0;
    font-size: 16px;
    font-weight: 600;
    color: var(--text-primary, #f5f5f5);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .when {
    margin: 4px 0 0;
    font-size: 13px;
    color: var(--text-secondary, #9aa);
  }
  .muted { color: var(--text-secondary, #6b7280); margin-left: 4px; }

  .chips {
    display: grid;
    grid-template-columns: repeat(5, minmax(0, 1fr));
    gap: 6px;
  }
  .chip {
    padding: 8px 0;
    border: 1px solid var(--border-color, #333);
    background: var(--bg-secondary, #2a2a2a);
    color: var(--text-primary, #e5e5e5);
    border-radius: 6px;
    font-size: 13px;
    cursor: pointer;
    transition: background 0.1s ease, border-color 0.1s ease;
  }
  .chip:hover:not(:disabled) {
    background: var(--bg-hover, #333);
    border-color: var(--border-strong, #555);
  }
  .chip:disabled { opacity: 0.5; cursor: default; }

  .custom .lbl {
    display: block;
    font-size: 12px;
    color: var(--text-secondary, #888);
    margin-bottom: 6px;
  }
  .custom .row {
    display: flex;
    gap: 6px;
    align-items: center;
  }
  .custom .row input[type="number"] {
    flex: 0 0 80px;
    min-width: 0;
  }
  .custom .row select {
    flex: 0 0 auto;
  }
  .custom .row .btn {
    margin-left: auto;
    flex: 0 0 auto;
  }
  input[type="number"], select {
    padding: 8px 10px;
    border: 1px solid var(--border-color, #333);
    background: var(--bg-secondary, #2a2a2a);
    color: var(--text-primary, #e5e5e5);
    border-radius: 6px;
    font-size: 14px;
  }
  input[type="number"]:focus, select:focus {
    outline: 2px solid var(--accent, #3b82f6);
    outline-offset: -1px;
    border-color: transparent;
  }

  .btn {
    padding: 8px 14px;
    border: 1px solid transparent;
    border-radius: 6px;
    font-size: 14px;
    cursor: pointer;
  }
  .btn.primary {
    background: var(--accent, #3b82f6);
    color: #fff;
  }
  .btn.primary:hover:not(:disabled) { filter: brightness(1.08); }
  .btn.primary:disabled { opacity: 0.5; cursor: default; }
  .btn.ghost {
    background: transparent;
    border-color: var(--border-color, #333);
    color: var(--text-primary, #e5e5e5);
  }
  .btn.ghost:hover:not(:disabled) {
    background: var(--bg-hover, #2a2a2a);
  }
  .btn.wide { width: 100%; }

  .atstart { }

  .footer {
    margin-top: auto;
    display: flex;
    justify-content: flex-end;
    gap: 8px;
  }

  .skel, .err {
    padding: 24px 8px;
    text-align: center;
    color: var(--text-secondary, #888);
  }
  .err { color: var(--text-error, #ef4444); }
</style>
