//! UI-thread state shared by every handler (`Shared`), and the two ways to
//! reach the UI from code that cannot capture it (`SHARED`, `UI_WEAK`).

use super::*;

/// UI-thread state shared by the select/resize/engine-result paths. All mail
/// state is interior-mutable so the live engine refresh can replace it.
pub(crate) struct Shared {
    pub(crate) cache: Option<Arc<Cache>>,
    pub(crate) key: String,
    /// Склеенный вид (merges.json применён) — то, что показывает сайдбар;
    /// все индексы UI указывают сюда.
    pub(crate) convs: RefCell<Vec<Conversation>>,
    /// Сырой список от движка/кэша, ДО применения склеек. Дельта-мерж
    /// Conversations работает по нему (id склейки — синтетический и в
    /// дельтах не встречается), merge/unmerge пересобирают convs из него.
    pub(crate) raw_convs: RefCell<Vec<Conversation>>,
    /// Пользовательские объединения диалогов; персистятся в merges.json.
    pub(crate) merges: RefCell<merges::Merges>,
    /// Вложение под правым кликом (folder, uid, index, filename) — цель
    /// пунктов «Открыть/Сохранить вложение» контекстного меню пузыря.
    pub(crate) ctx_attach: RefCell<Option<(String, u32, usize, String)>>,
    pub(crate) displays: RefCell<Vec<Disp>>,
    pub(crate) avatars: RefCell<HashMap<String, Image>>,
    /// Message refs for the currently rendered rows (row index → message).
    pub(crate) current_msgs: RefCell<Vec<MessageRef>>,
    /// Bodies of the open conversation, kept in memory (parallel to
    /// `current_msgs`) so resize / reply / forward / policy-toggle /
    /// send-subject paths never re-read SQLite on the UI thread.
    pub(crate) current_bodies: RefCell<Vec<MessageBody>>,
    /// Conversation-open generation. Bumped on every open_conversation;
    /// FetchMessages echoes it back so an answer for a conversation the
    /// user already left is dropped instead of overwriting the screen
    /// (same pattern as `search_query_inflight`).
    pub(crate) open_gen: Cell<u64>,
    /// (folder, uid) of the messages that were UNREAD when the current
    /// conversation was opened — the scroll anchor survives the
    /// mark-as-read that fires right after open.
    pub(crate) open_unread: RefCell<HashSet<(String, u32)>>,
    /// True until the first render after open has applied its scroll;
    /// lets the network Messages path scroll when the cache had nothing.
    pub(crate) scroll_pending: Cell<bool>,
    /// Где стоял скролл чата, когда мы уходили из почты в календарь/книгу.
    /// Положительный отступ от верха; -1 = ещё не уходили.
    ///
    /// Пока пользователь был не в почте, в ОТКРЫТЫЙ диалог пришло письмо.
    /// Перерисовать панель было некому — её не существовало, — поэтому
    /// возврат в почту должен не восстанавливать позицию, а открыть диалог
    /// заново: иначе новое письмо не появится в переписке вовсе.
    pub(crate) missed_mail: Cell<bool>,
    /// Диалог, открытый последним: пишется в calendar.json при каждом
    /// открытии, чтобы следующий запуск вернулся к нему, а не к первому в
    /// списке. Ключ — `Conversation::id` (набор адресов), он переживает и
    /// пересинк, и смену порядка списка.
    pub(crate) last_conv_id: RefCell<String>,
    /// Панель почты условная (`if root.view-mode == 0` в app.slint), а `if` в
    /// Slint уничтожает поддерево — возврат даёт новый ListView с viewport-y = 0.
    /// Без этого снимка диалог открывался в начале, а не там, где его
    /// оставили (для непрочитанного — не на свежем письме).
    pub(crate) chat_vp_y: Cell<f32>,
    /// Per-message render-view override: present = force the text-only
    /// bubble even when an HTML part exists («Показать → Текстовую
    /// версию»). Session-scoped on purpose.
    pub(crate) body_view_text: RefCell<HashSet<(String, u32)>>,
    /// Which source view a pending FetchSource should open:
    /// 1 = заголовки, 2 = полный исходник.
    pub(crate) pending_source_view: Cell<u8>,
    /// Full, untruncated text currently behind the source viewer (the widget
    /// shows only a capped slice — see SOURCE_VIEW_MAX). «Копировать всё» reads
    /// this so the clipboard always gets the complete source.
    pub(crate) source_view_full: RefCell<String>,
    /// email(lowercase) → пастельный цвет айдентики (подкраска строк
    /// сайдбара по received_by). Обновляется при каждом списке диалогов.
    pub(crate) identity_colors: RefCell<HashMap<String, String>>,
    /// UI-thread copy of the per-row link rects (CSS px, bubble-relative) —
    /// the only copy: the hover cursor, the link click (`on_hit_test`) and
    /// the context-menu probe all hit-test against it synchronously, without
    /// a round-trip to the render worker.
    pub(crate) row_links: RefCell<Vec<Vec<render_common::LinkRect>>>,
    /// What the shared confirmation modal confirms: 1 = удалить диалог,
    /// 2 = спам (blacklist + purge отправителя).
    pub(crate) confirm_mode: Cell<u8>,
    /// Per-row text layers (word rects, bubble-relative CSS px) — mouse
    /// selection. Parallel to the rendered rows, like row_links.
    pub(crate) row_text_runs: RefCell<Vec<Vec<render_common::TextRun>>>,
    /// Ссылка под курсором на момент показа контекстного меню и приложения,
    /// умеющие её открыть (имя + .desktop). Список читается один раз при старте.
    pub(crate) ctx_link: RefCell<Option<String>>,
    pub(crate) link_apps: Vec<(String, std::path::PathBuf)>,
    /// Toast-click navigation: scroll to this (folder, uid) once its body
    /// is rendered. Takes priority over the unread-anchor logic.
    pub(crate) pending_open_ref: RefCell<Option<(String, u32)>>,
    /// Render-job sequence shared with the render worker (see Job::seq).
    pub(crate) render_seq: Arc<AtomicU64>,
    pub(crate) current: Cell<usize>,
    pub(crate) width: Cell<u32>,
    /// UI window scale factor last seen by the width watcher; render jobs
    /// carry it so the renderer rasterizes at the display's real DPI.
    pub(crate) render_scale: Cell<f32>,
    pub(crate) tx: mpsc::Sender<Job>,
    pub(crate) engine_tx: RefCell<Option<mpsc::Sender<engine::EngineCmd>>>,
    /// Content-permission policy (per-sender media/scripts, per-domain
    /// allowlist) — port of the svelte permissionStore. Persisted to
    /// disk on every toggle.
    pub(crate) policy: RefCell<policy::Policy>,
    /// Monotonic generation counter, bumped each time the policy
    /// mutates. Render worker uses it as part of the bitmap cache key
    /// so toggling a permission invalidates exactly the relevant
    /// cached rows.
    pub(crate) policy_gen: Cell<u64>,
    // Per-feature state, one struct each (defined below).
    pub(crate) compose: ComposeState,
    pub(crate) selection: SelectionState,
    pub(crate) search: SearchState,
    pub(crate) cal: CalendarState,
    pub(crate) contacts: AddressBookState,
    pub(crate) accounts: AccountsState,
}

thread_local! {
    /// Set once on the UI thread so engine-result closures (posted via
    /// invoke_from_event_loop, which must be Send + 'static and can't capture
    /// the Rc) can reach the shared state.
    pub(crate) static SHARED: RefCell<Option<Rc<Shared>>> = const { RefCell::new(None) };
}

/// UI weak handle reachable from non-UI threads (toast click callbacks hop
/// to the event loop through it).
pub(crate) static UI_WEAK: std::sync::OnceLock<slint::Weak<MainWindow>> =
    std::sync::OnceLock::new();

/// Composer state: sender choice, staged reply/forward/new-mail target,
/// optimistic sends, attachments, the rich-text document.
pub(crate) struct ComposeState {
    /// Явно выбранный в дропдауне отправитель (lowercase email). Закрепляет
    /// пользовательский выбор: побеждает авто-наведение на identity беседы и
    /// переустановку индекса при дельта-refetch; on_send читает его
    /// приоритетно. None = явного выбора нет, действует авто-логика.
    /// Сбрасывается при смене контекста (открытие беседы / новое письмо).
    pub(crate) picked_identity: RefCell<Option<String>>,
    /// Диалог, в который надо перейти, когда он появится в списке: отправка с
    /// другого адреса образует свой набор адресов, то есть свою беседу, и она
    /// возникает не мгновенно — сначала письмо должно долететь до «Отправленных»
    /// и вернуться синком. Ставится галочкой в диалоге «не тот адрес».
    pub(crate) pending_switch: RefCell<Option<String>>,

    /// Отправка, задержанная диалогом «отвечаешь не с того адреса»: текст
    /// письма ждёт решения. Set → показан диалог; on_send при повторном входе
    /// забирает текст отсюда и проверку уже не делает.
    ///
    /// Задержка нужна потому, что композер очищает поле сразу при отправке:
    /// без этого отменённое письмо просто пропало бы.
    pub(crate) held_send: RefCell<Option<String>>,
    /// Forward target — set by «Переслать»; on Send the original's text
    /// goes below the typed text and its attachments are re-attached.
    pub(crate) pending_forward: RefCell<Option<MessageBody>>,
    /// From-picker дропдауна композера: e-mail'ы в том же порядке, что и
    /// Slint-модель composer-identities. on_send резолвит выбранный индекс
    /// через этот список (Slint-модель — источник только для отрисовки).
    pub(crate) composer_identities: RefCell<Vec<String>>,
    /// "Transient compose" target — set when the user picks a fresh
    /// recipient via the search dropdown ("Написать xxx@yyy" or a
    /// contact with no existing conversation). While Some, the chat
    /// pane shows an empty bubble list with the recipient pinned in
    /// the header; `on_send` routes the outgoing message to this
    /// address instead of the (irrelevant) sidebar-selected
    /// conversation. Cleared by EngineResult::Sent.
    pub(crate) pending_compose: RefCell<Option<String>>,
    /// Explicit-reply target — set when the user hits "Ответить" on a
    /// specific bubble. Drives the quote ribbon above the input and,
    /// at send time, the Re: subject + In-Reply-To / References
    /// threading. Cleared by Send or by the ribbon's × button.
    pub(crate) pending_reply: RefCell<Option<MessageBody>>,
    /// Optimistic-send stubs: an outgoing bubble goes into the open pane
    /// the moment «Отправить» is clicked, mirrored here so it can be
    /// reconciled (dropped when the real message comes back in a
    /// FetchMessages answer) or rolled back (send failed → bubble removed,
    /// text restored to the composer). Cleared on conversation switch —
    /// the stub lives and dies with the pane it was drawn in.
    pub(crate) pending_sends: RefCell<Vec<PendingSend>>,
    /// Synthetic uid source for stub bodies (folder = PENDING_FOLDER);
    /// unique within the session so render-cache keys never collide.
    pub(crate) pending_send_seq: Cell<u32>,
    /// Recipient of a just-sent transient compose. Set by
    /// EngineResult::Sent, consumed by the Conversations handler: as soon
    /// as the delta brings the (possibly brand-new) conversation row, the
    /// UI redirects to it instead of leaving the user on the stub pane.
    pub(crate) compose_sent_target: RefCell<Option<String>>,
    /// Files staged for the next outgoing message, picked via the composer's
    /// attach button. Parallel to the `composer-attachments` Slint model
    /// (which holds just the basenames). Cleared once a message is staged.
    pub(crate) compose_attachments: RefCell<Vec<std::path::PathBuf>>,
    /// Rich-text документ композера — источник истины для тела письма
    /// (Slint-свойство `composer-text` лишь его plain-зеркало).
    pub(crate) rich: RefCell<richtext::Editor>,
    /// Вёрстка/растеризация композера. Ленивая: сборка `FontSystem` читает
    /// системные шрифты (сотни мс), а композер нужен не в первую секунду.
    pub(crate) rich_renderer: RefCell<Option<richtext_render::Renderer>>,
    /// Ширина колонки текста, логические px — приходит из Slint (`rt-resize`).
    pub(crate) rich_width: Cell<f32>,
    /// Идёт протяжка выделения мышью.
    pub(crate) rich_dragging: Cell<bool>,
    /// Источник уникальных Content-ID для вставленных картинок.
    pub(crate) rich_cid_seq: Cell<u64>,
}

/// Mouse selection over bubble bitmaps and over the source viewer.
pub(crate) struct SelectionState {
    /// Word rects of the rendered source bitmap + its selection state. Mirrors
    /// the bubble selection layer (row_text_runs/sel_*) but for the modal.
    pub(crate) src_runs: RefCell<Vec<render_common::TextRun>>,
    pub(crate) src_sel_anchor: Cell<usize>,
    pub(crate) src_sel_head: Cell<usize>,
    pub(crate) src_sel_moved: Cell<bool>,
    pub(crate) src_sel_dragging: Cell<bool>,
    /// Mouse selection: row index (-1 none) and the anchor/head word
    /// indices within that row's text layer (inclusive, unordered).
    pub(crate) sel_row: Cell<i32>,
    pub(crate) sel_anchor: Cell<usize>,
    pub(crate) sel_head: Cell<usize>,
    pub(crate) sel_dragging: Cell<bool>,
    pub(crate) sel_moved: Cell<bool>,
    /// Set when a drag-selection just ended — the click that Slint fires
    /// on release must NOT open a link.
    pub(crate) sel_suppress_click: Cell<bool>,
    /// Серия кликов для выделения слова (второй) и строки (третий). Slint даёт
    /// только `clicked`, ни двойного, ни тройного события у него нет, поэтому
    /// серию считаем сами по нажатиям: время, строка и точка предыдущего.
    pub(crate) sel_click_streak: Cell<u32>,
    pub(crate) sel_click_at: Cell<Option<Instant>>,
    pub(crate) sel_click_pos: Cell<(i32, f32, f32)>,
}

/// The search dropdown's latest query and its three result lists.
pub(crate) struct SearchState {
    /// Last search query we asked the engine for. Engine echoes the
    /// query back in `SearchDropdown`; we drop results that don't match
    /// — handles the race where typing outruns the engine.
    pub(crate) search_query_inflight: RefCell<String>,
    /// Latest rows in the dropdown (parallel to the Slint model order),
    /// so callbacks can resolve `search-select-contact(idx)` and
    /// `search-select-message(idx)` back to their domain objects.
    pub(crate) search_contacts: RefCell<Vec<Contact>>,
    pub(crate) search_messages: RefCell<Vec<MessageEnvelope>>,
    /// Секция «Диалоги» — локальные совпадения (`local_search_convs`).
    pub(crate) search_convs: RefCell<Vec<ConvHit>>,
}

/// Calendar view, event card/form and reminder state.
pub(crate) struct CalendarState {
    /// Calendars list as the engine last reported it; we hold them so
    /// the visibility map can resolve names/colors when the user
    /// toggles checkboxes.
    pub(crate) calendars: RefCell<Vec<ddmail_core::types::DesktopCalendar>>,
    /// Per-calendar visibility, keyed by id. Defaults to true the first
    /// time a calendar shows up.
    pub(crate) calendar_visible: RefCell<HashMap<i64, bool>>,
    /// User-picked colour overrides (id → "#rrggbb"); wins over the server
    /// colour and the palette default. Persisted in calendar.json.
    pub(crate) calendar_colors: RefCell<HashMap<i64, String>>,
    /// Latest events from the engine, kept so toggling visibility /
    /// changing hour-range can re-layout without a server round-trip.
    pub(crate) calendar_events: RefCell<Vec<ddmail_core::types::DesktopCalendarEvent>>,
    /// First day of the currently displayed week (Monday) in local
    /// time, as days since the unix epoch. Stored as i64 so the
    /// timezone-conversion math is straightforward.
    pub(crate) calendar_week_start_days: Cell<i64>,
    /// Сетка стоит на «этой» неделе не потому, что её туда увели, а потому что
    /// это сегодняшняя неделя — значит при перекате суток её надо подтянуть.
    /// Ставится на каждом явном смещении недели (`неделя == сегодняшняя`), в
    /// момент проверки сравнить уже нельзя: после переката отображаемая неделя
    /// в любом случае не равна сегодняшней, и «оставили сами» от «убежало
    /// время» не отличить.
    pub(crate) week_follows_today: Cell<bool>,
    /// Live size of the calendar grid body (px), mirrored from Slint so the
    /// layout math can decide day-count / hour-height / what to hide.
    pub(crate) grid_canvas_w: Cell<f32>,
    pub(crate) grid_canvas_h: Cell<f32>,
    /// Working-day window (local hours) — the band kept visible when 0–24
    /// can't fit; outside it is shaded. Configurable in settings.
    pub(crate) work_start: Cell<i32>,
    pub(crate) work_end: Cell<i32>,
    /// Manual zoom (px); 0 = automatic fit. Set on ctrl / ctrl-alt scroll,
    /// after which manual zoom wins over autofit (per spec).
    pub(crate) manual_hour_h: Cell<f32>,
    pub(crate) manual_col_w: Cell<f32>,
    /// Event being edited (0 in create mode).
    pub(crate) editing_event_id: Cell<i64>,
    /// Форма события отправлена и ждёт ответа движка. Карточка закрывается по
    /// подтверждению, а не по факту нажатия: закрываясь сразу, она уносила с
    /// собой и отказ сервера — успех и провал выглядели одинаково (ничего).
    pub(crate) pending_event_save: Cell<bool>,
    /// Writable calendar ids, parallel to the edit-form's ComboBox model.
    pub(crate) edit_cal_ids: RefCell<Vec<i64>>,
    /// account_key of each writable calendar (parallel to edit_cal_ids), so a
    /// newly-created event routes to the calendar's owning account.
    pub(crate) edit_cal_accounts: RefCell<Vec<String>>,
    /// event id → owning account_key, from the last events fetch, so calendar
    /// writes (rsvp/patch/delete) route to the right connection.
    pub(crate) event_accounts: RefCell<HashMap<i64, String>>,
    /// Event a reminder toast asked to open (0 = none); consumed once the
    /// calendar events for its week arrive from the engine. The occurrence
    /// start + summary ride along for the stale-id fallback: the server
    /// re-creates events under new ids on calendar re-sync, so the id a
    /// reminder was seeded with may be dead by the time the toast is
    /// clicked — the meeting is then recovered by occurrence instead.
    pub(crate) pending_open_event: Cell<i64>,
    pub(crate) pending_open_occ: Cell<i64>,
    pub(crate) pending_open_summary: RefCell<String>,
    /// Last CalendarUpdated-driven refetch — debounces the server's push
    /// bursts (one per calendar per sync cycle) to one refetch per window.
    pub(crate) last_cal_refetch: Cell<Option<std::time::Instant>>,
    /// Hour the calendar grid should scroll to on the next layout (None =
    /// no request). Set when the calendar view opens; consumed by
    /// `apply_calendar_view` once real calendar data has arrived, so the
    /// scroll target is computed against the final hour-height, not the
    /// pre-data defaults. Re-issued on every apply until then.
    pub(crate) pending_cal_scroll: Cell<Option<f32>>,
    /// (event_id, occurrence_start_ms, occurrence_end_ms, toast_id, summary)
    /// the snooze modal is acting on. toast_id lets a committed choice close
    /// the originating toast and a cancel resume its paused timer.
    pub(crate) snooze_ctx: RefCell<(i64, i64, i64, u64, String)>,
    /// Per-render map of on-screen occurrences for drag-move: (event_id, day)
    /// → (occurrence_start_ms, occurrence_end_ms, recurring). Lets the drag
    /// handler recover the EXACT instance start (recurrence_id for scope=single)
    /// — Slint `int` is i32 and can't carry epoch-ms. Rebuilt each layout.
    pub(crate) cal_occ: RefCell<HashMap<(i32, i32), (i64, i64, bool)>>,
}

/// Address book list and the contact editor.
pub(crate) struct AddressBookState {
    /// Last-fetched address book (parallel to the `address-book` Slint model),
    /// so the contact editor can read a row's full data by index.
    pub(crate) address_book: RefCell<Vec<ddmail_core::types::DesktopContact>>,
    /// Contact being edited (0 in create mode).
    pub(crate) editing_contact_id: Cell<i64>,
    /// account_key of the contact under edit, for multi-account write routing
    /// (empty ⇒ the engine falls back to the first account).
    pub(crate) editing_contact_account: RefCell<String>,
    /// account_keys parallel to the contact editor's account ComboBox (create).
    pub(crate) ce_account_keys: RefCell<Vec<String>>,
}

/// Connections: which one the open conversation belongs to, their states
/// and re-login prompts, and the add/edit connection window.
pub(crate) struct AccountsState {
    /// account_key of the currently open conversation. Addressed engine
    /// commands (body/flags/delete/source/attachment/send) carry it so they
    /// route to the right server. Empty falls back to the primary account.
    pub(crate) cur_account_key: RefCell<String>,
    /// All account keys (the indicator's denominator) and their last-known
    /// connection state ("connecting" | "connected" | "error" | "auth").
    /// Drives the aggregate green/yellow/red status light.
    pub(crate) account_keys: RefCell<Vec<String>>,
    pub(crate) account_states: RefCell<HashMap<String, String>>,
    /// Ключи строк списка учёток под индикатором связи (порядок
    /// accounts.json) — по индексу строки `relogin` находит, какую учётку
    /// открывать в форме входа.
    pub(crate) conn_dot_keys: RefCell<Vec<String>>,
    /// Учётки, чья сессия мертва и ждёт пароля — в порядке accounts.json.
    /// Отдельно от `account_states`, потому что состояние липкое: watcher
    /// после отказа ещё успевает крикнуть "connecting"/"error", и в общей
    /// карте «нужен вход» тут же затиралось бы на «нет связи». Снимается
    /// только удачным коннектом, ротацией токена или пересборкой движка.
    pub(crate) reauth: RefCell<Vec<String>>,
    /// Keeps the add/edit-connection modal alive while it's open.
    pub(crate) add_conn_window: RefCell<Option<LoginWindow>>,
    /// account_keys parallel to the settings connections list (for edit/delete
    /// by row index).
    pub(crate) settings_conn_keys: RefCell<Vec<String>>,
}
