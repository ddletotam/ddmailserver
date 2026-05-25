<script lang="ts">
  import { invoke } from "@tauri-apps/api/core";
  import { accountStore } from "../stores/accounts.svelte";
  import { calendarStore } from "../stores/calendar.svelte";
  import type { DesktopCalendarEvent } from "../types/calendar";

  interface Props {
    /** Existing event to edit. Omit for create-mode. */
    event?: DesktopCalendarEvent | null;
    occStart: number; // ms
    occEnd: number;
    /** Calendar to seed the picker with in create-mode. */
    initialCalendarId?: number | null;
    onclose: () => void;
    onsaved: () => void;
  }
  let { event = null, occStart, occEnd, initialCalendarId = null, onclose, onsaved }: Props = $props();

  const isCreate = $derived(!event);
  const isRecurring = $derived(!!event?.rrule);

  // Calendar picker for create-mode: only writable AND currently visible to
  // the user. The user almost never wants to create on a calendar they've
  // hidden, and a read-only one would error on save anyway.
  const writableVisibleCalendars = $derived(
    calendarStore.calendars.filter((c) => c.can_write && calendarStore.isVisible(c.id)),
  );
  let selectedCalendarId = $state<number | null>(
    initialCalendarId ?? null,
  );
  $effect(() => {
    // Seed once when the writable+visible list materialises and nothing's
    // chosen. Don't overwrite the user's explicit pick.
    if (selectedCalendarId == null && writableVisibleCalendars.length > 0) {
      selectedCalendarId = writableVisibleCalendars[0].id;
    }
  });

  // Form fields seeded from the *instance* being edited, not the master's
  // DTSTART. For a non-recurring event occStart == event.dtstart so they're
  // identical; for a recurring instance, the user expects the form to show
  // the date of the occurrence they double-clicked. In create-mode all
  // fields start empty/from the clicked cell.
  let summary = $state(event?.summary ?? "");
  let description = $state(event?.description ?? "");
  let location = $state(event?.location ?? "");
  let allDay = $state(event?.all_day ?? false);
  let startStr = $state(toInputValue(occStart, allDay));
  let endStr = $state(toInputValue(occEnd, allDay));

  $effect(() => {
    // Switching all-day toggle remaps the input format (date-only vs datetime).
    // Re-seed from current state values rather than the original event so the
    // user doesn't lose what they typed.
    const sMs = fromInputValue(startStr, !allDay);
    const eMs = fromInputValue(endStr, !allDay);
    startStr = toInputValue(sMs || occStart, allDay);
    endStr = toInputValue(eMs || occEnd, allDay);
  });

  function pad(n: number): string { return n < 10 ? "0" + n : String(n); }

  function toInputValue(ms: number, isDateOnly: boolean): string {
    const d = new Date(ms);
    const yyyy = d.getFullYear();
    const mm = pad(d.getMonth() + 1);
    const dd = pad(d.getDate());
    if (isDateOnly) return `${yyyy}-${mm}-${dd}`;
    // Space separator (not "T") so the seeded value matches the placeholder
    // the user sees — `Date.parse` accepts both forms.
    return `${yyyy}-${mm}-${dd} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  }

  // wasDateOnly param controls how we parse — datetime-local strings include
  // a "T", date strings don't. Returns 0 on a parse failure so the caller can
  // skip the field rather than save a 1970 timestamp.
  function fromInputValue(v: string, _wasDateOnly: boolean): number {
    if (!v) return 0;
    const parsed = Date.parse(v);
    return Number.isFinite(parsed) ? parsed : 0;
  }

  // ── Scope picker ──
  // Shows after Save for recurring events. Non-recurring events skip
  // straight to the PATCH with scope="all".
  let showScope = $state(false);
  let saving = $state(false);
  let errorMsg = $state<string | null>(null);

  async function doSave(scope: "all" | "future" | "single") {
    const account = accountStore.activeAccount;
    if (!account) return;
    saving = true;
    errorMsg = null;
    const sMs = fromInputValue(startStr, allDay);
    const eMs = fromInputValue(endStr, allDay);
    try {
      if (isCreate) {
        if (!selectedCalendarId) {
          errorMsg = "Выберите календарь";
          return;
        }
        const body: Record<string, unknown> = {
          calendar_id: selectedCalendarId,
          summary,
          description,
          location,
          all_day: allDay,
          dtstart: sMs || occStart,
        };
        if (eMs) body.dtend = eMs;
        await invoke("v2_create_event", { accountId: account.id, body });
      } else {
        const body: Record<string, unknown> = {
          scope,
          summary,
          description,
          location,
          all_day: allDay,
        };
        if (scope !== "all") body.recurrence_id = occStart;
        if (sMs) body.dtstart = sMs;
        body.dtend = eMs; // explicit 0 ⇒ clear end on the server
        await invoke("v2_patch_event", {
          accountId: account.id,
          eventId: event!.id,
          body,
        });
      }
      await calendarStore.refreshAfterRSVP(account);
      onsaved();
    } catch (e: unknown) {
      errorMsg = e instanceof Error ? e.message : String(e);
      console.error("[event-edit] save failed:", e);
    } finally {
      saving = false;
    }
  }

  function handleSave() {
    if (isCreate) {
      doSave("all"); // scope irrelevant for create
    } else if (isRecurring) {
      showScope = true;
    } else {
      doSave("all");
    }
  }

  function keyHandler(e: KeyboardEvent) {
    if (e.key === "Escape") {
      if (showScope) showScope = false;
      else onclose();
    }
  }
</script>

<svelte:window onkeydown={keyHandler} />

<!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
<div class="backdrop" onclick={(e) => { if (e.target === e.currentTarget) onclose(); }}>
  <div class="card" role="dialog" aria-modal="true" aria-label="Редактировать событие">
    <header class="card-head">
      <h2>{isCreate ? "Новое событие" : "Редактировать"}</h2>
      <button class="btn-close" onclick={onclose} aria-label="Закрыть">×</button>
    </header>

    <div class="form">
      {#if isCreate}
        <label class="field">
          <span class="label">Календарь</span>
          {#if writableVisibleCalendars.length === 0}
            <div class="err">Нет доступных для записи и видимых календарей. Включите хотя бы один в левой панели.</div>
          {:else}
            <select bind:value={selectedCalendarId}>
              {#each writableVisibleCalendars as c}
                <option value={c.id}>{c.name}</option>
              {/each}
            </select>
          {/if}
        </label>
      {/if}

      <label class="field">
        <span class="label">Название</span>
        <input type="text" bind:value={summary} />
      </label>

      <label class="field row">
        <span class="label">Весь день</span>
        <input type="checkbox" bind:checked={allDay} />
      </label>

      <!-- Type=text instead of date/datetime-local because WebKitGTK
           (Tauri/Linux) ships a broken native picker for those: the
           popup opens but lacks OK/Cancel and the value never commits.
           Date.parse handles both `YYYY-MM-DD` and `YYYY-MM-DDTHH:MM`,
           so the on-save parse path is unchanged. -->
      <div class="field-pair">
        <label class="field">
          <span class="label">Начало</span>
          <input
            type="text"
            inputmode="numeric"
            placeholder={allDay ? "ГГГГ-ММ-ДД" : "ГГГГ-ММ-ДД ЧЧ:ММ"}
            bind:value={startStr}
          />
        </label>
        <label class="field">
          <span class="label">Окончание</span>
          <input
            type="text"
            inputmode="numeric"
            placeholder={allDay ? "ГГГГ-ММ-ДД" : "ГГГГ-ММ-ДД ЧЧ:ММ"}
            bind:value={endStr}
          />
        </label>
      </div>

      <label class="field">
        <span class="label">Место</span>
        <input type="text" bind:value={location} />
      </label>

      <label class="field">
        <span class="label">Описание</span>
        <textarea rows="5" bind:value={description}></textarea>
      </label>

      {#if errorMsg}<div class="err">{errorMsg}</div>{/if}
    </div>

    <footer class="card-foot">
      <button class="btn-cancel" onclick={onclose} disabled={saving}>Отмена</button>
      <button class="btn-save" onclick={handleSave} disabled={saving}>
        {saving ? "Сохраняю…" : "Сохранить"}
      </button>
    </footer>
  </div>

  {#if showScope}
    <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
    <div class="scope-backdrop" onclick={(e) => { if (e.target === e.currentTarget) showScope = false; }}>
      <div class="scope-card" role="dialog" aria-modal="true" aria-label="Область изменения">
        <h3>Применить к…</h3>
        <p class="scope-hint">Это повторяющееся событие.</p>
        <div class="scope-btns">
          <button
            class="scope-pill"
            onclick={() => doSave("all")}
            disabled={saving}
          >Ко всем повторениям</button>
          <button
            class="scope-pill"
            onclick={() => doSave("future")}
            disabled={saving}
          >К этому и последующим</button>
          <button
            class="scope-pill disabled-pill"
            onclick={() => doSave("single")}
            disabled={saving}
            title="Пока не реализовано на сервере"
          >Только к этому (TODO)</button>
        </div>
      </div>
    </div>
  {/if}
</div>

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    background: rgba(0, 0, 0, 0.5);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 1100;
  }
  .card {
    background: var(--bg-primary);
    color: var(--text-primary);
    border-radius: 12px;
    box-shadow: var(--shadow-md);
    width: min(560px, 92vw);
    max-height: 88vh;
    display: flex;
    flex-direction: column;
    overflow: hidden;
    font-family: var(--font-family);
    font-size: var(--font-size);
  }
  .card-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 14px 20px;
    border-bottom: 1px solid var(--border-color);
  }
  .card-head h2 { margin: 0; font-size: 16px; font-weight: 600; }
  .btn-close {
    width: 28px; height: 28px;
    border: none; background: none;
    color: var(--text-secondary);
    font-size: 22px; line-height: 1;
    cursor: pointer; border-radius: 50%;
  }
  .btn-close:hover { background: var(--bg-hover); color: var(--text-primary); }

  .form {
    padding: 16px 20px;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    gap: 12px;
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .field.row {
    flex-direction: row;
    align-items: center;
    justify-content: space-between;
  }
  .field-pair {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 12px;
  }
  .label {
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  input[type="text"], input[type="date"], input[type="datetime-local"], textarea, select {
    border: 1px solid var(--border-color);
    border-radius: 8px;
    padding: 8px 10px;
    background: var(--bg-primary);
    color: var(--text-primary);
    font-family: inherit;
    font-size: var(--font-size);
    outline: none;
  }
  input:focus, textarea:focus, select:focus { border-color: var(--text-accent); }
  textarea { resize: vertical; min-height: 80px; }

  .err {
    color: #c03939;
    font-size: var(--font-size-sm);
  }

  .card-foot {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
    padding: 12px 20px;
    border-top: 1px solid var(--border-color);
  }
  .btn-cancel, .btn-save {
    border: 1px solid var(--border-color);
    background: var(--bg-primary);
    color: var(--text-primary);
    border-radius: 8px;
    padding: 8px 16px;
    cursor: pointer;
    font: inherit;
  }
  .btn-cancel:hover { background: var(--bg-hover); }
  .btn-save {
    background: var(--text-accent);
    color: var(--text-on-active);
    border-color: var(--text-accent);
  }
  .btn-save:disabled, .btn-cancel:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }

  /* Scope picker */
  .scope-backdrop {
    position: fixed;
    inset: 0;
    background: rgba(0, 0, 0, 0.4);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 1200;
  }
  .scope-card {
    background: var(--bg-primary);
    border-radius: 12px;
    box-shadow: var(--shadow-md);
    padding: 20px;
    width: min(380px, 90vw);
  }
  .scope-card h3 { margin: 0 0 6px; font-size: 16px; }
  .scope-hint { margin: 0 0 14px; color: var(--text-secondary); font-size: var(--font-size-sm); }
  .scope-btns { display: flex; flex-direction: column; gap: 8px; }
  .scope-pill {
    border: 1px solid var(--border-color);
    background: var(--bg-primary);
    color: var(--text-primary);
    padding: 10px 14px;
    border-radius: 10px;
    cursor: pointer;
    text-align: left;
    font: inherit;
  }
  .scope-pill:hover { background: var(--bg-hover); }
  .scope-pill.disabled-pill { color: var(--text-secondary); }
</style>
