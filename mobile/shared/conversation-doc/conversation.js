// Лента пузырей открытой беседы (контракт Android §8). Общий для мобильных
// клиентов: нативный код только отдаёт письма и исполняет просьбы моста.
//
// Нативный → документ:  ddm.set(batch), ddm.prepend(batch), ddm.append(batch)
//   batch = { messages: [Msg], hasOlder: bool, focusUid?: number }
//   Msg   = { uid, outgoing, fromName, fromAddr, dateTs (сек), html?, text?,
//             attachments: [{index, filename, size}], showFrom, delivery? }
//                       ddm.stub(Stub), ddm.unstub(key)  — optimistic send
//   Stub  = { key, html?, text, attachments: [{filename, size}] } — html есть, если композер с разметкой
//                       ddm.delivery(uid, Delivery)       — статус письма сменился (на месте)
//                       ddm.stubDelivery(key, Delivery)   — статус заглушки (IMAP: SMTP отказал)
//   Delivery = { status, error?, failed?: [адрес] } — доставка своего исходящего:
//     status `pending` («Отправляется…»; с error — «Не удалось отправить,
//     повторяем: …»), `failed` (красное «Не доставлено: …», failed — кому),
//     `delivered`, пусто или нет поля — обычное письмо, без пометки.
// Документ → нативный:  ready(), openLink(url), requestOlder(),
//   openAttachment(uid, index), attachmentMenu(uid, index), bubbleMenu(uid),
//   allowSender(uid), allowSpamImages(uid),
//   selectionMenu(x, y, w, h) — только WKWebView: показать меню над выделением
//     (прямоугольник в CSS px от начала документа)
//   WKWebView к bubbleMenu и attachmentMenu добавляет ещё шесть чисел
//     (x, y, w, h, px, py): прямоугольник пузыря (чипа) и точку нажатия в CSS
//     px от начала документа — нативное меню встаёт у пузыря. Android их не
//     получает: у `@JavascriptInterface` число аргументов — часть подписи.
// Нативный → документ (выделение): ddm.keyboard(on), ddm.selectedText(),
//   ddm.selectionRect()
// Нативный → документ (меню пузыря): ddm.remove([uid]) — письма удалены,
//   ddm.view(uid, 'text'|'html') — текстовая версия письма / снова HTML.
//   Оба необязательны: нативный код проверяет их наличие.
//   Msg.imagesAllowed — картинки этого отправителя разрешены всегда.
//   Msg.spam — письмо из «Спама»: картинки только по нажатию на этот показ
//     (нативный пропуск — allowSpamImages), «Всегда от отправителя» нет,
//     внешние шрифты и @import вырезаются (контракт Android §5 «Спам»).
'use strict';

/** Сменилась нормализация — поднять: клиенты пересчитают закэшированное. */
const NORMALIZER_VERSION = 2;

const native = (() => {
  if (window.ddmNative) {
    return (name, ...args) => window.ddmNative[name](...args);
  }
  const wk = window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.ddm;
  if (wk) return (name, ...args) => wk.postMessage({ name, args });
  return () => {};
})();

/**
 * Долгое нажатие → меню; обработчик получает точку нажатия (client px).
 * Android WebView и Мак (правый клик) присылают
 * `contextmenu`; WKWebView на iPhone/iPad при долгом нажатии его не шлёт, а
 * начинает выделение текста — меню пузыря и вложения были недостижимы.
 * Поэтому на сенсорном WebKit долгое нажатие считаем сами: 450 мс без
 * сдвига пальца дальше 10 px, затем гасим начатое выделение и следующий клик.
 */
const touchWebKit = !window.ddmNative && !!(window.webkit && window.webkit.messageHandlers) &&
  ('ontouchstart' in window);
const LONG_PRESS_MS = 450;
function onLongPress(el, handler) {
  // Chromium шлёт `contextmenu` и на тап по уже выделенному тексту (меню
  // «Копировать»): это не долгое нажатие — палец уже поднят или опущен
  // только что. Тогда меню системы, а не письма.
  let downAt = 0;
  if (!touchWebKit && 'ontouchstart' in window) {
    el.addEventListener('touchstart', () => { downAt = Date.now(); }, { passive: true, capture: true });
    el.addEventListener('touchend', () => { downAt = 0; }, { passive: true, capture: true });
    el.addEventListener('touchcancel', () => { downAt = 0; }, { passive: true, capture: true });
  }
  el.addEventListener('contextmenu', (e) => {
    const sel = window.getSelection();
    if ('ontouchstart' in window && !touchWebKit && sel && sel.toString() &&
        (!downAt || Date.now() - downAt < LONG_PRESS_MS - 100)) return;
    e.preventDefault(); e.stopPropagation();
    // Chromium при активном выделении переносит его на слово под пальцем —
    // меню письма открывается без выделения, как на iPhone.
    if (sel && sel.toString()) sel.removeAllRanges();
    handler(e.clientX, e.clientY);
  });
  if (!touchWebKit) return;
  let timer = null, x = 0, y = 0, fired = false;
  const cancel = () => { if (timer) { clearTimeout(timer); timer = null; } };
  el.addEventListener('touchstart', (e) => {
    if (e.touches.length !== 1) { cancel(); return; }
    e.stopPropagation();
    fired = false;
    x = e.touches[0].clientX; y = e.touches[0].clientY;
    cancel();
    // Палец на маркере выделения — его тянут, это не долгое нажатие.
    if (nearSelectionHandle(x, y)) return;
    timer = setTimeout(() => {
      timer = null; fired = true;
      holdNativeSelection(true);
      handler(x, y);
    }, LONG_PRESS_MS);
  }, { passive: true });
  el.addEventListener('touchmove', (e) => {
    const t = e.touches[0];
    if (t && Math.hypot(t.clientX - x, t.clientY - y) > 10) cancel();
  }, { passive: true });
  const up = () => { cancel(); if (fired) holdNativeSelection(false); };
  el.addEventListener('touchend', up, { passive: true });
  el.addEventListener('touchcancel', up, { passive: true });
  // Палец отпущен после срабатывания — клик не должен открыть вложение/ссылку.
  el.addEventListener('click', (e) => { if (fired) { fired = false; e.preventDefault(); e.stopPropagation(); } }, true);
}
if (touchWebKit) document.documentElement.classList.add('ddm-touch-webkit');

/**
 * Просьба о нативном меню у элемента (пузыря, чипа вложения): после `args` —
 * прямоугольник элемента и точка нажатия.
 * - WebKit (iPhone, iPad, Мак): в CSS px от начала документа (как
 *   `selectionMenu`) — меню встаёт у пузыря, а не посреди колонки.
 * - Android: в CSS px от левого верхнего угла видимой области (visual
 *   viewport: учтены прокрутка и щипок) и последним — её ширина в CSS px;
 *   отношение ширины WebView в пикселях к ней — масштаб (density × щипок).
 *   Число аргументов входит в сигнатуру `@JavascriptInterface` — менять
 *   вместе с `ConversationWeb.Bridge`.
 */
function askMenu(name, args, el, x, y) {
  const r = el.getBoundingClientRect();
  const px = typeof x === 'number' ? x : r.left + r.width / 2;
  const py = typeof y === 'number' ? y : r.top + r.height / 2;
  if (window.ddmNative) {
    const vv = window.visualViewport;
    const ox = vv ? vv.offsetLeft : 0, oy = vv ? vv.offsetTop : 0;
    const vw = vv ? vv.width : window.innerWidth;
    native(name, ...args, r.left - ox, r.top - oy, r.width, r.height, px - ox, py - oy, vw);
    return;
  }
  native(name, ...args, r.left + scrollX, r.top + scrollY, r.width, r.height, px + scrollX, py + scrollY);
}

/**
 * Долгое нажатие сработало (меню письма), а WebKit чуть позже начинает своё
 * выделение слова с системным меню «Скопировать / Найти» — поверх нашего.
 * Пока палец держат (не дольше 1,5 с) и полсекунды после — текст не выделяется
 * (`user-select: none` на документе), а то, что всё же выделилось, снимается.
 */
let nativeSelectionHeld = 0;
function holdNativeSelection(on) {
  const root = document.documentElement;
  if (on) {
    nativeSelectionHeld = Infinity;
    root.classList.add('ddm-noselect');
    const sel = window.getSelection();
    if (sel) sel.removeAllRanges();
    // Нативное меню забирает палец себе — `touchend` может не прийти.
    setTimeout(() => { if (nativeSelectionHeld === Infinity) holdNativeSelection(false); }, 1500);
    return;
  }
  const until = Date.now() + 500;
  nativeSelectionHeld = until;
  setTimeout(() => {
    if (nativeSelectionHeld !== until) return;
    nativeSelectionHeld = 0;
    root.classList.remove('ddm-noselect');
  }, 500);
}
document.addEventListener('selectionchange', () => {
  if (Date.now() >= nativeSelectionHeld) return;
  const sel = window.getSelection();
  if (sel && sel.rangeCount) sel.removeAllRanges();
});

// ── Выделение текста двойным и тройным тапом ──────────────────────────────
//
// Долгое нажатие занято меню письма (onLongPress), поэтому выделение
// начинается как у десктопа серией касаний (контракт десктопа §4в): второй
// тап — слово, третий — фраза (предложение; без сегментатора — строка до
// переноса, то есть абзац или строка письма). Дальше — системные маркеры и
// «Скопировать». Мышь и трекпад (Мак, iPad с трекпадом) этим не заняты:
// там двойной и тройной клик выделяют сами.
//
// Тело письма — в закрытом shadow root пузыря: точку в текст переводит
// `caretPositionFromPoint` с `shadowRoots`, где его нет — свой поиск символа
// под пальцем по прямоугольникам (`elementFromPoint` у shadow root есть
// везде). Границы слова и предложения — `Intl.Segmenter`.

const TAP_SLOP = 10;        // px: дальше — это уже прокрутка, не тап
const TAP_MAX_MS = 350;     // дольше — не тап (а к 450 мс — долгое нажатие)
const SERIES_GAP_MS = 400;  // пауза между тапами серии
const SERIES_SLOP = 30;     // px: тапы серии — в одно место

const segmenter = (granularity) => {
  try { return new Intl.Segmenter('ru', { granularity }); } catch (_) { return null; }
};
const wordSegmenter = segmenter('word');
const sentenceSegmenter = segmenter('sentence');

/** Расстояние от точки до прямоугольника (0 — внутри). */
function rectDistance(r, x, y) {
  const dx = x < r.left ? r.left - x : x > r.right ? x - r.right : 0;
  const dy = y < r.top ? r.top - y : y > r.bottom ? y - r.bottom : 0;
  return Math.hypot(dx, dy);
}

/** Видимый текстовый узел: в свёрнутой цитате текст есть, но не нарисован. */
function renderedText(t) {
  const p = t.parentElement;
  return !!p && p.getClientRects().length > 0;
}

/**
 * Символ под точкой среди текста `scope` (узел и номер символа): ближайший
 * по прямоугольникам. Тап между строк или у края — ближайший символ.
 */
function charNear(scope, x, y) {
  const range = document.createRange();
  let best = null;
  let bestD = Infinity;
  let budget = 20000;
  const walker = document.createTreeWalker(scope, NodeFilter.SHOW_TEXT);
  for (let t = walker.nextNode(); t && budget > 0; t = walker.nextNode()) {
    if (!t.nodeValue.trim() || !renderedText(t)) continue;
    range.selectNodeContents(t);
    let nodeD = Infinity;
    for (const r of range.getClientRects()) nodeD = Math.min(nodeD, rectDistance(r, x, y));
    if (nodeD > bestD) continue;
    const n = t.nodeValue.length;
    budget -= n;
    for (let i = 0; i < n; i++) {
      range.setStart(t, i);
      range.setEnd(t, i + 1);
      const r = range.getBoundingClientRect();
      if (!r.width && !r.height) continue;
      const d = rectDistance(r, x, y);
      if (d < bestD) {
        bestD = d;
        best = { node: t, offset: i };
        if (d === 0) return best;
      }
    }
  }
  return best;
}

/** Символ под пальцем в письме или null (тап мимо текста). */
function charAt(shadow, x, y) {
  if (document.caretPositionFromPoint) {
    try {
      const p = document.caretPositionFromPoint(x, y, { shadowRoots: [shadow] });
      const n = p && p.offsetNode;
      if (n && n.nodeType === Node.TEXT_NODE && shadow.contains(n)) {
        // Каретка — между символами: символ под пальцем — тот, чей
        // прямоугольник ближе (справа или слева от каретки).
        const c = charNear(n.parentNode, x, y);
        if (c) return c;
      }
    } catch (_) { /* старый движок: shadowRoots не понимает */ }
  }
  const el = shadow.elementFromPoint(x, y);
  if (!el || !shadow.contains(el)) return null;
  return charNear(el, x, y);
}

const INLINE_DISPLAY = /^(inline|contents|ruby)/;

/** Блок, в котором ищутся границы: ближайший не-строчный предок. */
function blockOf(node, root) {
  let el = node.parentElement;
  while (el && el !== root) {
    if (!INLINE_DISPLAY.test(getComputedStyle(el).display)) return el;
    el = el.parentElement;
  }
  return root;
}

/**
 * Текст блока строкой и куски — какой узел какой отрезок строки дал.
 * `<br>`, вложенные блоки и скрытый текст — перенос строки: границы
 * выделения через них не идут.
 */
function blockText(block) {
  let s = '';
  const pieces = [];
  const walker = document.createTreeWalker(block, NodeFilter.SHOW_TEXT | NodeFilter.SHOW_ELEMENT);
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    if (n.nodeType === Node.ELEMENT_NODE) {
      if (n.tagName === 'BR' || !INLINE_DISPLAY.test(getComputedStyle(n).display)) s += '\n';
      continue;
    }
    if (!renderedText(n)) { s += '\n'; continue; }
    pieces.push({ node: n, start: s.length, end: s.length + n.nodeValue.length });
    s += n.nodeValue;
  }
  return { s, pieces };
}

/** Позиция в строке блока → точка DOM (`atEnd` — конец отрезка, а не начало). */
function domPoint(pieces, pos, atEnd) {
  for (const p of pieces) {
    if (atEnd ? (pos > p.start && pos <= p.end) : (pos >= p.start && pos < p.end)) {
      return { node: p.node, offset: pos - p.start };
    }
  }
  return null;
}

/** Отрезок сегмента, в который попала позиция `i` строки `s`. */
function segmentAt(seg, s, i) {
  for (const x of seg.segment(s)) {
    if (i >= x.index && i < x.index + x.segment.length) return [x.index, x.index + x.segment.length, x];
  }
  return null;
}

/**
 * Границы выделения в строке `line` вокруг символа `i`: слово (без
 * сегментатора — буквы и цифры подряд) или предложение (без сегментатора —
 * вся строка). Пробелы по краям не берём.
 */
function boundsIn(line, i, kind) {
  let a = 0, b = line.length;
  if (kind === 'word') {
    if (wordSegmenter) {
      const hit = segmentAt(wordSegmenter, line, i);
      if (hit) [a, b] = hit;
    } else {
      const w = /[\p{L}\p{N}_]/u;
      a = i; b = i + 1;
      if (w.test(line[i])) {
        while (a > 0 && w.test(line[a - 1])) a--;
        while (b < line.length && w.test(line[b])) b++;
      }
    }
  } else if (sentenceSegmenter) {
    const hit = segmentAt(sentenceSegmenter, line, i);
    if (hit) [a, b] = hit;
  }
  while (a < b && /\s/.test(line[a])) a++;
  while (b > a && /\s/.test(line[b - 1])) b--;
  return b > a ? [a, b] : null;
}

/** Последнее письмо, в котором выделяли серией тапов (для маркеров). */
let selectionShadow = null;

/**
 * Выделить слово (`word`) или фразу (`sentence`) под точкой в письме.
 * Ответ — выделенный Range или null (тап мимо текста).
 */
function selectAt(shadow, root, x, y, kind) {
  const c = charAt(shadow, x, y);
  if (!c) return null;
  const { s, pieces } = blockText(blockOf(c.node, root));
  const piece = pieces.find((p) => p.node === c.node);
  if (!piece) return null;
  const i = piece.start + c.offset;
  const ls = s.lastIndexOf('\n', i - 1) + 1;
  const le = s.indexOf('\n', i) < 0 ? s.length : s.indexOf('\n', i);
  const b = boundsIn(s.slice(ls, le), i - ls, kind);
  if (!b) return null;
  const from = domPoint(pieces, ls + b[0], false);
  const to = domPoint(pieces, ls + b[1], true);
  if (!from || !to) return null;
  const r = document.createRange();
  r.setStart(from.node, from.offset);
  r.setEnd(to.node, to.offset);
  const sel = window.getSelection();
  try {
    sel.setBaseAndExtent(from.node, from.offset, to.node, to.offset);
  } catch (_) {
    sel.removeAllRanges();
    sel.addRange(r);
  }
  selectionShadow = shadow;
  return r;
}

/**
 * Выделение внутри письма — как Range (для его прямоугольников) или null.
 * Письмо — то, где выделяли серией тапов, иначе любое из ленты (выделение
 * мог поставить и сам движок).
 */
function selectionRange() {
  const sel = window.getSelection();
  if (!sel || sel.rangeCount === 0 || typeof sel.getComposedRanges !== 'function') return null;
  const roots = [];
  if (selectionShadow && selectionShadow.host.isConnected) roots.push(selectionShadow);
  for (const e of bubbles.values()) if (e.shadow !== selectionShadow) roots.push(e.shadow);
  if (!roots.length) return null;
  const inside = (n) => roots.find((r) => r.contains(n));
  let ranges = null;
  try { ranges = sel.getComposedRanges({ shadowRoots: roots }); } catch (_) { /* старая подпись */ }
  if (!ranges || !ranges.length || !inside(ranges[0].startContainer)) {
    try { ranges = sel.getComposedRanges(...roots); } catch (_) { return null; }
  }
  const sr = ranges && ranges[0];
  if (!sr || sr.collapsed) return null;
  const root = inside(sr.startContainer);
  if (!root) return null;
  const r = document.createRange();
  try {
    r.setStart(sr.startContainer, sr.startOffset);
    r.setEnd(sr.endContainer, sr.endOffset);
  } catch (_) { return null; }
  selectionShadow = root;
  return r;
}

/**
 * Клавиатура открыта (фокус в строке ввода; говорит нативная часть,
 * `ddm.keyboard`). Тогда WKWebView второй тап серии документу не отдаёт и
 * выделяет слово сам — без меню. Такое выделение документ замечает по
 * `selectionchange` и просит меню у нативной части, как для своего.
 */
let keyboardUp = false;
let selectionMenuTimer = 0;
if (touchWebKit) {
  document.addEventListener('selectionchange', () => {
    if (!keyboardUp) return;
    clearTimeout(selectionMenuTimer);
    selectionMenuTimer = setTimeout(() => {
      const r = selectionRange();
      if (!r || r.collapsed) return;
      const b = r.getBoundingClientRect();
      native('selectionMenu', b.left + scrollX, b.top + scrollY, b.width, b.height);
    }, 300);
  });
}

/**
 * Палец у маркера выделения (начало или конец): долгое нажатие тогда не
 * перехватываем — маркер тянут. Где выделение не узнать (нет
 * `getComposedRanges`), любое непустое выделение отдаёт палец системе.
 */
function nearSelectionHandle(x, y) {
  const sel = window.getSelection();
  if (!sel || sel.isCollapsed && !sel.toString()) return false;
  const r = selectionRange();
  if (!r) return !!sel.toString();
  if (r.collapsed) return false;
  const rects = [...r.getClientRects()].filter((q) => q.width || q.height);
  if (!rects.length) return false;
  const first = rects[0];
  const last = rects[rects.length - 1];
  const R = 36;
  return Math.hypot(x - first.left, y - (first.top + first.bottom) / 2) < R ||
    Math.hypot(x - last.right, y - (last.top + last.bottom) / 2) < R;
}

/**
 * Серия тапов по телу письма: второй тап — слово, третий — предложение.
 *
 * WKWebView (iPhone/iPad): тап серии гасим на `touchend` (`preventDefault`:
 * ни клика по ссылке, ни масштаба) и выделяем сами — маркеры и меню WebKit
 * показывает и для выделения из скрипта.
 *
 * Chromium (Android WebView): выделение из скрипта он рисует без маркеров и
 * меню, а тап по уже выделенному тексту показывает их (и шлёт `contextmenu`,
 * его пропускает onLongPress). Поэтому выделяем на `touchstart` тапа серии —
 * сам тап приходится уже на выделение. Сорвался тап в прокрутку — снимаем.
 *
 * Клики серии (по ссылкам в том числе) не проходят: открывает только
 * одиночный тап.
 */
function wireTapSeries(host, shadow, root) {
  if (!('ontouchstart' in window)) return;
  let count = 0, lastEnd = 0, lx = 0, ly = 0;
  let sx = 0, sy = 0, st = 0, moved = true, early = false, swallowClickUntil = 0;
  const inSeries = (now) => now - lastEnd < SERIES_GAP_MS && Math.hypot(sx - lx, sy - ly) < SERIES_SLOP;
  const kindOf = (n) => (n === 2 ? 'word' : 'sentence');
  host.addEventListener('touchstart', (e) => {
    early = false;
    moved = e.touches.length !== 1;
    if (moved) { count = 0; return; }
    sx = e.touches[0].clientX; sy = e.touches[0].clientY; st = Date.now();
    if (!touchWebKit && count >= 1 && inSeries(st)) {
      early = !!selectAt(shadow, root(), sx, sy, kindOf(count + 1));
      if (early) swallowClickUntil = st + TAP_MAX_MS + SERIES_GAP_MS;
    }
  }, { passive: true });
  host.addEventListener('touchmove', (e) => {
    const t = e.touches[0];
    if (moved || !t || Math.hypot(t.clientX - sx, t.clientY - sy) <= TAP_SLOP) return;
    moved = true;
    if (early) { window.getSelection().removeAllRanges(); early = false; }
  }, { passive: true });
  host.addEventListener('touchcancel', () => { moved = true; count = 0; }, { passive: true });
  host.addEventListener('touchend', (e) => {
    const now = Date.now();
    if (moved || now - st > TAP_MAX_MS) { count = 0; return; }
    count = inSeries(now) ? count + 1 : 1;
    lastEnd = now; lx = sx; ly = sy;
    if (count < 2) return;
    swallowClickUntil = now + SERIES_GAP_MS;
    if (early) return;
    if (e.cancelable) e.preventDefault();
    const x = sx, y = sy, kind = kindOf(count);
    const select = () => {
      const r = selectAt(shadow, root(), x, y, kind);
      // Меню «Скопировать» у выделения из скрипта WebKit не показывает —
      // его показывает нативная часть (ConversationWeb.swift) над выделением.
      if (r && touchWebKit) {
        const b = r.getBoundingClientRect();
        native('selectionMenu', b.left + scrollX, b.top + scrollY, b.width, b.height);
      }
      return r;
    };
    if (!select() || !touchWebKit) return;
    // Была открыта клавиатура (фокус в строке ввода): WebKit, забирая фокус
    // после первого тапа, следом снимает выделение, а уход клавиатуры
    // закрывает меню — тогда ставим их ещё раз, пока фокус не устоится.
    // Следующий тап серии (лишнее касание) отменяет повтор.
    const at = lastEnd;
    let redone = false;
    for (const ms of [350, 900]) {
      setTimeout(() => {
        if (lastEnd !== at) return;
        if (!ddm.selectedText()) redone = true;
        if (redone) select();
      }, ms);
    }
  }, { passive: false });
  host.addEventListener('click', (e) => {
    if (Date.now() < swallowClickUntil) { e.preventDefault(); e.stopPropagation(); }
  }, true);
}

const feed = document.getElementById('feed');
const olderEl = document.getElementById('older');

/**
 * Иконки Material (24×24) — вместо надписей: на мобильных иконки, а не
 * слова (контракт §0). Инлайн-SVG — CSP не мешает, сеть не нужна.
 */
const ICONS = {
  image: 'M21 19V5c0-1.1-.9-2-2-2H5c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h14c1.1 0 2-.9 2-2zM8.5 13.5l2.5 3.01L14.5 12l4.5 6H5l3.5-4.5z',
  trust: 'M12 1L3 5v6c0 5.55 3.84 10.74 9 12 5.16-1.26 9-6.45 9-12V5l-9-4zm-2 16l-4-4 1.41-1.41L10 14.17l6.59-6.59L18 9l-8 8z',
  schedule: 'M11.99 2C6.47 2 2 6.48 2 12s4.47 10 9.99 10C17.52 22 22 17.52 22 12S17.52 2 11.99 2zM12 20c-4.42 0-8-3.58-8-8s3.58-8 8-8 8 3.58 8 8-3.58 8-8 8zm.5-13H11v6l5.25 3.15.75-1.23-4.5-2.67z',
  error: 'M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm1 15h-2v-2h2v2zm0-4h-2V7h2v6z',
  attach: 'M16.5 6v11.5c0 2.21-1.79 4-4 4s-4-1.79-4-4V5c0-1.38 1.12-2.5 2.5-2.5s2.5 1.12 2.5 2.5v10.5c0 .55-.45 1-1 1s-1-.45-1-1V6H10v9.5c0 1.38 1.12 2.5 2.5 2.5s2.5-1.12 2.5-2.5V5c0-2.21-1.79-4-4-4S7 2.79 7 5v12.5c0 3.04 2.46 5.5 5.5 5.5s5.5-2.46 5.5-5.5V6h-1.5z',
};

function icon(name) {
  const NS = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('aria-hidden', 'true');
  svg.classList.add('ddm-icon');
  const path = document.createElementNS(NS, 'path');
  path.setAttribute('d', ICONS[name]);
  svg.appendChild(path);
  return svg;
}
const scroller = document.scrollingElement;

/** uid → { msg, host, root, imagesAllowed } — для перерисовки и подгонки ширины. */
const bubbles = new Map();
/** Письма, показанные текстовой версией (меню «Показать… → Текстовую версию»). */
const textViews = new Set();
/** Письма в порядке ленты (старые → новые). */
let order = [];
let hasOlder = false;
let loadingOlder = false;

// ── Нормализатор ──────────────────────────────────────────────────────────

const DROP = 'script,iframe,frame,frameset,object,embed,applet,form,base,meta,link,noscript,title,audio,video,source,track,portal,template';
const URL_ATTRS = ['href', 'src', 'action', 'formaction', 'background', 'poster', 'xlink:href', 'cite', 'longdesc'];
const QUOTE_SELECTORS = [
  '.gmail_quote', '.gmail_quote_container', '.yahoo_quoted', '.moz-cite-prefix',
  'blockquote[type="cite"]', '#mail-app-auto-quote', '.protonmail_quote',
];
// Выбранные по тексту разделители пересылки/ответа (Outlook, Thunderbird, Яндекс, Mail.ru).
const QUOTE_MARKERS = [
  /^-{2,}\s*Original Message\s*-{2,}/i,
  /^-{2,}\s*Forwarded message\s*-{2,}/i,
  /^-{2,}\s*Пересылаемое сообщение\s*-{2,}/i,
  /^-{2,}\s*Исходное сообщение\s*-{2,}/i,
  /^-{2,}\s*Начало переадресованного сообщения/i,
];

/** Не даём письму прибить что-то к экрану поверх ленты и интерфейса. */
function tameCss(css) {
  return css.replace(/position\s*:\s*(fixed|sticky)/gi, 'position:static');
}

function isTrackingPixel(img) {
  const w = img.getAttribute('width');
  const h = img.getAttribute('height');
  const tiny = (v) => v !== null && /^\s*[01](px)?\s*$/i.test(v);
  if (tiny(w) && tiny(h)) return true;
  const st = (img.getAttribute('style') || '').replace(/\s+/g, '');
  return /width:[01]px/i.test(st) && /height:[01]px/i.test(st);
}

/**
 * Внешний URL: до разрешения картинок не грузится вовсе, после — только через
 * нативный прокси с источника документа (CSP пускает только его).
 */
function remoteUrl(u, imagesAllowed, uid) {
  if (!imagesAllowed) return null;
  // uid — для нативного пропуска: у спама он пускает только письма, которым
  // нажали «Показать картинки».
  return '/remote?uid=' + uid + '&u=' + encodeURIComponent(u);
}

/**
 * Спам: из CSS — вон всё, что тянет не картинки: `@import` (в том числе
 * строкой, без `url()`) и `@font-face`. Перехватчик и так пустит только
 * картинки; это — чтобы не было и попыток.
 */
function stripSpamCss(css) {
  return css
    .replace(/@import\b[^;]*;?/gi, '')
    .replace(/@font-face\s*\{[^}]*\}/gi, '');
}

function rewriteCssUrls(css, uid, imagesAllowed) {
  return css.replace(/url\(\s*(['"]?)([^'")]+)\1\s*\)/gi, (all, q, u) => {
    const url = u.trim();
    if (/^cid:/i.test(url)) return `url("/cid/${uid}/${encodeURIComponent(url.slice(4))}")`;
    if (/^https?:/i.test(url)) {
      const r = remoteUrl(url, imagesAllowed, uid);
      return r ? `url("${r}")` : 'none';
    }
    if (/^data:image\//i.test(url)) return all;
    return 'none';
  });
}

/**
 * Чистка в инертном документе (DOMParser ничего не исполняет и не грузит).
 * Возвращает { styles: [css], body: Element, remoteCount }.
 */
function sanitize(html, uid, imagesAllowed, spam) {
  // <base> вырезаем до разбора: иначе CSP документа (`base-uri 'none'`)
  // ругается на каждое такое письмо, хотя тег и так не дожил бы до вставки.
  const doc = new DOMParser().parseFromString(html.replace(/<base\b[^>]*>/gi, ''), 'text/html');
  const styles = [];
  doc.querySelectorAll('style').forEach((s) => {
    const css = tameCss(s.textContent || '');
    styles.push(rewriteCssUrls(spam ? stripSpamCss(css) : css, uid, imagesAllowed));
    s.remove();
  });
  doc.querySelectorAll(DROP).forEach((el) => el.remove());

  let remoteCount = 0;
  for (const el of doc.body.querySelectorAll('*')) {
    for (const attr of [...el.attributes]) {
      const name = attr.name.toLowerCase();
      if (name.startsWith('on') || name === 'srcset' || name === 'ping') {
        el.removeAttribute(attr.name);
        continue;
      }
      if (URL_ATTRS.includes(name)) {
        const v = attr.value.trim();
        if (/^(javascript|vbscript|data:text\/html)/i.test(v.replace(/[\s\u0000-\u001f]/g, ''))) {
          el.removeAttribute(attr.name);
        }
      }
    }
    const st = el.getAttribute('style');
    if (st) el.setAttribute('style', rewriteCssUrls(tameCss(st), uid, imagesAllowed));
    const bg = el.getAttribute('background');
    if (bg) {
      el.removeAttribute('background');
      if (/^https?:/i.test(bg)) remoteCount++;
    }
  }

  for (const img of doc.body.querySelectorAll('img')) {
    if (isTrackingPixel(img)) {
      img.remove();
      continue;
    }
    const src = (img.getAttribute('src') || '').trim();
    if (/^cid:/i.test(src)) {
      img.setAttribute('src', `/cid/${uid}/${encodeURIComponent(src.slice(4))}`);
    } else if (/^https?:/i.test(src)) {
      remoteCount++;
      const r = remoteUrl(src, imagesAllowed, uid);
      if (r) {
        img.setAttribute('src', r);
      } else {
        img.removeAttribute('src');
        img.classList.add('ddm-blocked');
        // Место под картинку — по атрибутам, чтобы вёрстка не прыгала после загрузки.
        const w = parseInt(img.getAttribute('width'), 10);
        const h = parseInt(img.getAttribute('height'), 10);
        if (w > 0) img.style.width = w + 'px';
        if (h > 0) img.style.height = h + 'px';
      }
    } else if (!/^data:image\//i.test(src)) {
      img.removeAttribute('src');
    }
  }
  return { styles, body: doc.body, remoteCount };
}

function details(nodes, doc) {
  const d = doc.createElement('details');
  d.className = 'ddm-quote';
  const s = doc.createElement('summary');
  s.textContent = '•••';
  d.appendChild(s);
  for (const n of nodes) d.appendChild(n);
  return d;
}

/** Блок верхнего уровня (прямой ребёнок body), в котором лежит узел. */
function topBlock(body, node) {
  let n = node;
  while (n && n.parentNode !== body) n = n.parentNode;
  return n;
}

/**
 * Сворачивание цитат: известные обёртки цитат и всё, что идёт после
 * разделителя пересылки/ответа. Внутри уже свёрнутого второй раз не сворачиваем.
 */
function collapseQuotes(body) {
  const doc = body.ownerDocument;
  for (const el of body.querySelectorAll(QUOTE_SELECTORS.join(','))) {
    if (!el.isConnected || el.closest('details.ddm-quote')) continue;
    // У Thunderbird строка «… писал:» — отдельный div перед blockquote.
    const nodes = [el];
    if (el.matches('.moz-cite-prefix') && el.nextElementSibling && el.nextElementSibling.matches('blockquote')) {
      nodes.push(el.nextElementSibling);
    }
    const marker = doc.createComment('q');
    el.parentNode.insertBefore(marker, el);
    marker.replaceWith(details(nodes, doc));
  }

  // Outlook: div#divRplyFwdMsg (или #appendonsend) и всё после него в своём контейнере.
  const outlook = body.querySelector('#divRplyFwdMsg, #appendonsend');
  if (outlook && !outlook.closest('details.ddm-quote')) {
    const nodes = [];
    let start = outlook;
    const prev = outlook.previousElementSibling;
    if (prev && prev.tagName === 'HR') start = prev;
    for (let n = start; n; n = n.nextSibling) nodes.push(n);
    const d = details([], doc);
    start.parentNode.insertBefore(d, start);
    for (const n of nodes) d.appendChild(n);
  }

  // Текстовые разделители: блок с ним и всё после него до конца письма.
  const walker = doc.createTreeWalker(body, NodeFilter.SHOW_TEXT);
  for (let t = walker.nextNode(); t; t = walker.nextNode()) {
    const text = t.nodeValue.trim();
    if (!text) continue;
    const marker = QUOTE_MARKERS.some((re) => re.test(text)) || isBareSeparator(t, text);
    if (!marker) continue;
    if (t.parentNode.closest && t.parentNode.closest('details.ddm-quote')) continue;
    const block = topBlock(body, t);
    if (!block) break;
    const nodes = [];
    for (let n = block; n; n = n.nextSibling) nodes.push(n);
    const d = details([], doc);
    body.insertBefore(d, block);
    for (const n of nodes) d.appendChild(n);
    break;
  }
}

/**
 * Разделитель из одних дефисов (Mail.ru, Яндекс, часть Outlook) — цитата,
 * только если сразу за ним идут заголовки пересылаемого письма: иначе это
 * просто черта в оформлении.
 */
const HEADER_AFTER = /^(Кому|От|Тема|Дата|Отправлено|Копия|From|To|Subject|Date|Sent|Cc)\s*:/im;

function isBareSeparator(node, text) {
  if (!/^[-_—=]{8,}$/.test(text)) return false;
  let after = '';
  const walker = node.ownerDocument.createTreeWalker(node.ownerDocument.body, NodeFilter.SHOW_TEXT);
  walker.currentNode = node;
  for (let n = walker.nextNode(); n && after.length < 300; n = walker.nextNode()) after += n.nodeValue + '\n';
  return HEADER_AFTER.test(after.trim().split('\n').slice(0, 3).join('\n'));
}

function escapeHtml(s) {
  return s.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function linkify(escaped) {
  return escaped.replace(/\bhttps?:\/\/[^\s<>"']+[^\s<>"'.,;:!?)\]]/g, (u) => `<a href="${u}">${u}</a>`);
}

/**
 * Текстовое письмо → разметка. Строки «>» — цитата; хвост письма из цитаты
 * (с предшествующей строкой «… писал(а):») сворачивается.
 */
function textToHtml(text) {
  const lines = text.replace(/\r\n?/g, '\n').split('\n');
  let tail = lines.length;
  while (tail > 0 && (/^\s*>/.test(lines[tail - 1]) || lines[tail - 1].trim() === '')) tail--;
  const hasTailQuote = tail < lines.length && lines.slice(tail).some((l) => /^\s*>/.test(l));
  if (hasTailQuote && tail > 0 && /(wrote|писал|написал|пишет)[^:]*:\s*$/i.test(lines[tail - 1])) tail--;
  const render = (ls) => {
    const out = [];
    let quote = [];
    const flush = () => {
      if (quote.length) out.push(`<div class="ddm-tquote">${quote.join('\n')}</div>`);
      quote = [];
    };
    for (const l of ls) {
      const e = linkify(escapeHtml(l));
      if (/^\s*>/.test(l)) quote.push(e);
      else {
        flush();
        out.push(e);
      }
    }
    flush();
    return out.join('\n');
  };
  const head = render(lines.slice(0, hasTailQuote ? tail : lines.length)).replace(/\s+$/, '');
  const quoted = hasTailQuote ? render(lines.slice(tail)) : '';
  return `<div class="ddm-text">${head}</div>` +
    (quoted ? `<details class="ddm-quote"><summary>•••</summary><div class="ddm-text">${quoted}</div></details>` : '');
}

// ── Пузыри ────────────────────────────────────────────────────────────────

const timeFmt = new Intl.DateTimeFormat('ru-RU', { hour: '2-digit', minute: '2-digit' });
const dayFmt = new Intl.DateTimeFormat('ru-RU', { day: 'numeric', month: 'long' });
const dayYearFmt = new Intl.DateTimeFormat('ru-RU', { day: 'numeric', month: 'long', year: 'numeric' });

function dayKey(ts) {
  const d = new Date(ts * 1000);
  return `${d.getFullYear()}-${d.getMonth()}-${d.getDate()}`;
}

function dayLabel(ts) {
  const d = new Date(ts * 1000);
  const now = new Date();
  if (dayKey(ts) === dayKey(now.getTime() / 1000)) return 'Сегодня';
  const y = new Date(now);
  y.setDate(now.getDate() - 1);
  if (dayKey(ts) === dayKey(y.getTime() / 1000)) return 'Вчера';
  return (d.getFullYear() === now.getFullYear() ? dayFmt : dayYearFmt).format(d);
}

function formatSize(n) {
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1).replace('.', ',') + ' МБ';
  if (n >= 1 << 10) return Math.round(n / (1 << 10)) + ' КБ';
  return n + ' Б';
}

/** Ссылки и кнопки внутри письма — в нативный код, документ никуда не уходит. */
function wireClicks(root) {
  root.addEventListener('click', (e) => {
    const a = e.target.closest && e.target.closest('a[href]');
    if (!a) return;
    e.preventDefault();
    const href = a.getAttribute('href').trim();
    if (/^(https?:|mailto:|tel:)/i.test(href)) native('openLink', href);
  });
}

function fill(entry) {
  const { msg, shadow } = entry;
  shadow.replaceChildren();
  const link = document.createElement('link');
  link.rel = 'stylesheet';
  link.href = 'bubble.css';
  shadow.appendChild(link);
  const root = document.createElement('div');
  root.className = 'ddm-root';
  let remoteCount = 0;
  if (msg.html && !entry.viewText) {
    const s = sanitize(msg.html, msg.uid, entry.imagesAllowed, !!msg.spam);
    remoteCount = s.remoteCount;
    collapseQuotes(s.body);
    for (const css of s.styles) {
      const st = document.createElement('style');
      st.textContent = css;
      shadow.appendChild(st);
    }
    root.replaceChildren(...s.body.childNodes);
  } else {
    root.innerHTML = textToHtml(msg.text || '');
  }
  shadow.appendChild(root);
  entry.root = root;
  entry.imagesBox.hidden = !(remoteCount > 0 && !entry.imagesAllowed);
  if (!entry.imagesBox.hidden) {
    // У спама — понятная плашка словами: картинки там не грузятся сами.
    const label = msg.spam ? `Показать картинки (${remoteCount})` : String(remoteCount);
    entry.imagesBtn.replaceChildren(icon('image'), label);
    entry.imagesBtn.setAttribute('aria-label', `Показать картинки: ${remoteCount}`);
  }
  link.addEventListener('load', () => fit(entry));
  root.querySelectorAll('img').forEach((img) => img.addEventListener('load', () => {
    fit(entry);
    keepAnchor();
  }));
  if (entry.host.isConnected) fit(entry);
}

/**
 * Письмо шире пузыря — масштабируем целиком (`zoom`), а не раскладываем в
 * столбик: в линеаризованной таблице цифры теряют связь со столбцами (§8).
 */
/**
 * Удержание позиции: письмо перерисовалось (картинки разрешили) и выросло —
 * то, на что смотрели, не должно уехать. Якорь — низ пузыря, на кнопке
 * которого нажали; держим его несколько секунд, пока грузятся картинки.
 */
let anchor = null;

function holdAnchor(row) {
  anchor = { row, bottom: row.getBoundingClientRect().bottom, until: Date.now() + 15000 };
}

function keepAnchor() {
  if (!anchor || Date.now() > anchor.until || !anchor.row.isConnected) {
    anchor = null;
    return;
  }
  const now = anchor.row.getBoundingClientRect().bottom;
  scroller.scrollTop += now - anchor.bottom;
}

function allowImages(entry) {
  // Спам: сначала нативный пропуск (вызов синхронный), потом перерисовка —
  // иначе перехватчик отказал бы картинкам, которые она запросит.
  if (entry.msg.spam) native('allowSpamImages', entry.msg.uid);
  holdAnchor(entry.row);
  entry.imagesAllowed = true;
  fill(entry);
  keepAnchor();
}

function fit(entry) {
  const { root, host } = entry;
  if (!root) return;
  root.style.zoom = '';
  root.classList.remove('ddm-scaled');
  const avail = host.clientWidth;
  const need = root.scrollWidth;
  if (avail > 0 && need > avail + 1) {
    root.classList.add('ddm-scaled');
    root.style.zoom = String(avail / need);
  }
}

// ── Статус доставки своего письма (контракт Android §5) ──────────────────

/** Причина посреди фразы: «…: почтовый сервер не отвечает» (аббревиатуры не трогаем). */
function reasonText(s) {
  const t = (s || '').trim();
  if (t.length > 1 && t[0] !== t[0].toLowerCase() && t[1] === t[1].toLowerCase()) return t[0].toLowerCase() + t.slice(1);
  return t;
}

/**
 * Пометка доставки в пузыре: `pending` — «Отправляется…» (с причиной —
 * «Не удалось отправить, повторяем: …»), `failed` — красное «Не доставлено:
 * …» и, если сервер назвал, кому. `delivered` и пусто — пометки нет.
 * Пузырь с отказом обведён красным (`ddm-failed` на строке).
 */
function renderDelivery(el, row, d) {
  const status = (d && d.status) || '';
  const error = reasonText(d && d.error);
  const failed = (d && Array.isArray(d.failed)) ? d.failed.filter(Boolean) : [];
  el.replaceChildren();
  el.className = 'ddm-dl';
  el.hidden = status !== 'pending' && status !== 'failed';
  row.classList.toggle('ddm-failed', status === 'failed');
  if (el.hidden) return;
  const line = document.createElement('div');
  line.className = 'ddm-dl-line';
  const text = document.createElement('span');
  if (status === 'failed') {
    el.classList.add('ddm-dl-failed');
    text.textContent = error ? `Не доставлено: ${error}` : 'Не доставлено';
    line.append(icon('error'), text);
  } else if (error) {
    el.classList.add('ddm-dl-retry');
    text.textContent = `Не удалось отправить, повторяем: ${error}`;
    line.append(icon('schedule'), text);
  } else {
    el.classList.add('ddm-dl-pending');
    text.textContent = 'Отправляется…';
    line.append(icon('schedule'), text);
  }
  el.appendChild(line);
  if (status === 'failed' && failed.length) {
    const who = document.createElement('div');
    who.className = 'ddm-dl-who';
    who.textContent = failed.join(', ');
    el.appendChild(who);
  }
}

/** Пометка поменяла высоту пузыря — кто был внизу ленты, остаётся внизу. */
function keepBottom(change) {
  const atBottom = scroller.scrollHeight - scroller.scrollTop - window.innerHeight < 80;
  change();
  if (atBottom) scroller.scrollTop = scroller.scrollHeight;
}

function makeRow(msg) {
  const row = document.createElement('div');
  row.className = 'ddm-row' + (msg.outgoing ? ' ddm-out' : '');
  row.dataset.uid = msg.uid;
  const b = document.createElement('div');
  b.className = 'ddm-bubble' + (msg.html ? ' ddm-wide' : '');
  if (msg.showFrom && !msg.outgoing) {
    const f = document.createElement('div');
    f.className = 'ddm-from';
    f.textContent = msg.fromName || msg.fromAddr;
    b.appendChild(f);
  }
  const host = document.createElement('div');
  host.className = 'ddm-host';
  b.appendChild(host);
  const shadow = host.attachShadow({ mode: 'closed' });
  wireClicks(shadow);

  const imagesBox = document.createElement('div');
  imagesBox.className = 'ddm-imgbar' + (msg.spam ? ' ddm-spambar' : '');
  imagesBox.hidden = true;
  const imagesBtn = document.createElement('button');
  imagesBtn.className = 'ddm-images';
  const alwaysBtn = document.createElement('button');
  alwaysBtn.className = 'ddm-images';
  alwaysBtn.append(icon('image'), icon('trust'));
  alwaysBtn.setAttribute('aria-label', 'Всегда показывать картинки от ' + (msg.fromAddr || 'отправителя'));
  // У спама «Всегда от отправителя» нет: его адрес ничего не доказывает.
  if (msg.spam) imagesBox.append(imagesBtn);
  else imagesBox.append(imagesBtn, alwaysBtn);
  b.appendChild(imagesBox);

  if (msg.attachments && msg.attachments.length) {
    const box = document.createElement('div');
    box.className = 'ddm-atts';
    for (const a of msg.attachments) {
      const chip = document.createElement('button');
      chip.className = 'ddm-att';
      const name = document.createElement('span');
      name.textContent = a.filename || 'вложение';
      const size = document.createElement('small');
      size.textContent = formatSize(a.size || 0);
      chip.append(icon('attach'), name, size);
      chip.addEventListener('click', () => native('openAttachment', msg.uid, a.index));
      // Долгое нажатие на чип — «Открыть» / «Поделиться» / «Сохранить».
      onLongPress(chip, (x, y) => askMenu('attachmentMenu', [msg.uid, a.index], chip, x, y));
      box.appendChild(chip);
    }
    b.appendChild(box);
  }
  const deliveryEl = document.createElement('div');
  renderDelivery(deliveryEl, row, msg.delivery);
  b.appendChild(deliveryEl);
  const meta = document.createElement('div');
  meta.className = 'ddm-meta';
  meta.textContent = msg.dateTs ? timeFmt.format(new Date(msg.dateTs * 1000)) : '';
  b.appendChild(meta);
  row.appendChild(b);

  // Долгое нажатие на пузырь — нативное меню (ответить, переслать, …),
  // у самого пузыря.
  onLongPress(row, (x, y) => askMenu('bubbleMenu', [msg.uid], b, x, y));

  const entry = {
    msg, host, shadow, root: null, imagesAllowed: !!msg.imagesAllowed, imagesBtn, imagesBox, row, deliveryEl,
    viewText: textViews.has(msg.uid),
  };
  // Двойной тап — слово, тройной — фраза; дальше системные маркеры.
  wireTapSeries(host, shadow, () => entry.root);
  // «Показать» — на это письмо до закрытия беседы; «Всегда» — запоминает
  // нативный код для отправителя (политика по отправителю, как у десктопа).
  imagesBtn.addEventListener('click', () => allowImages(entry));
  alwaysBtn.addEventListener('click', () => {
    if (msg.spam) return;
    native('allowSender', msg.uid);
    for (const e of bubbles.values()) {
      if (e.msg.fromAddr && e.msg.fromAddr === msg.fromAddr && !e.imagesAllowed && !e.msg.spam) allowImages(e);
    }
  });
  bubbles.set(msg.uid, entry);
  fill(entry);
  return row;
}

function daySep(ts) {
  const d = document.createElement('div');
  d.className = 'ddm-day';
  d.dataset.day = dayKey(ts);
  d.textContent = dayLabel(ts);
  return d;
}

/** Фрагмент из писем (по возрастанию даты) с разделителями дней. */
function build(msgs, prevTs) {
  const frag = document.createDocumentFragment();
  let last = prevTs ? dayKey(prevTs) : null;
  for (const m of msgs) {
    const k = dayKey(m.dateTs || 0);
    if (k !== last) frag.appendChild(daySep(m.dateTs || 0));
    last = k;
    frag.appendChild(makeRow(m));
  }
  return frag;
}

function byDate(a, b) {
  return (a.dateTs || 0) - (b.dateTs || 0) || a.uid - b.uid;
}

function setOlder(v) {
  hasOlder = !!v;
  olderEl.hidden = !hasOlder;
  olderEl.setAttribute('aria-label', 'Загружаем раньше');
}

function fitAll() {
  for (const e of bubbles.values()) fit(e);
}

// ── Optimistic send (§2 десктопа) ─────────────────────────────────────────

/**
 * key → { row, norm, failed } — заглушки отправляемых писем. `failed` —
 * письмо не ушло (IMAP: SMTP отказал): заглушка остаётся с пометкой, пока
 * нативный код её не уберёт, и с настоящим письмом не сверяется.
 */
const stubs = new Map();
const STUB_TTL_MS = 120000;

function normText(s) {
  return (s || '').replace(/\s+/g, ' ').trim();
}

function plainOf(m) {
  if (m.text) return m.text;
  if (!m.html) return '';
  return new DOMParser().parseFromString(m.html, 'text/html').body.textContent || '';
}

/** Заглушка выглядит как письмо: тот же HTML и те же чипы вложений. */
function makeStub(st) {
  const row = document.createElement('div');
  row.className = 'ddm-row ddm-out ddm-pending';
  const b = document.createElement('div');
  b.className = 'ddm-bubble';
  const host = document.createElement('div');
  host.className = 'ddm-host';
  const shadow = host.attachShadow({ mode: 'closed' });
  const link = document.createElement('link');
  link.rel = 'stylesheet';
  link.href = 'bubble.css';
  const root = document.createElement('div');
  root.className = 'ddm-root';
  // HTML — тот, что уходит на сервер (свой, из композера: текст экранирован,
  // картинки — data:-URI); без него — текст, как раньше. Скрипты режет CSP.
  root.innerHTML = st.html || textToHtml(st.text || '');
  shadow.append(link, root);
  b.appendChild(host);
  if (st.attachments && st.attachments.length) {
    const box = document.createElement('div');
    box.className = 'ddm-atts';
    for (const a of st.attachments) {
      const chip = document.createElement('span');
      chip.className = 'ddm-att';
      const name = document.createElement('span');
      name.textContent = a.filename;
      const size = document.createElement('small');
      size.textContent = formatSize(a.size || 0);
      chip.append('📎', name, size);
      box.appendChild(chip);
    }
    b.appendChild(box);
  }
  const meta = document.createElement('div');
  meta.className = 'ddm-meta';
  meta.textContent = 'Отправляется…';
  b.appendChild(meta);
  row.appendChild(b);
  return row;
}

/** Статус заглушки: пометка — вместо «Отправляется…» в подвале. */
function stubDelivery(st, d) {
  const meta = st.row.querySelector('.ddm-meta');
  let el = st.row.querySelector('.ddm-dl');
  if (!el) {
    el = document.createElement('div');
    meta.parentNode.insertBefore(el, meta);
  }
  renderDelivery(el, st.row, d);
  st.failed = !!d && d.status === 'failed';
  st.row.classList.toggle('ddm-pending', !st.failed);
  // Время отказа — вместо «Отправляется…»: письмо уже никуда не отправляется.
  meta.textContent = st.failed ? timeFmt.format(new Date()) : 'Отправляется…';
}

const ddm = {
  /** Новая беседа: всё заново. */
  set(batch) {
    bubbles.clear();
    textViews.clear();
    // Заглушки принадлежат прежней беседе: иначе append() вернул бы их в новую.
    stubs.clear();
    order = [...batch.messages].sort(byDate);
    feed.replaceChildren(build(order, null));
    setOlder(batch.hasOlder);
    loadingOlder = false;
    fitAll();
    const focus = batch.focusUid && feed.querySelector(`.ddm-row[data-uid="${batch.focusUid}"]`);
    if (focus) focus.scrollIntoView({ block: 'start' });
    else scroller.scrollTop = scroller.scrollHeight;
  },

  /** Более старые письма сверху — без прыжка того, на что смотрят. */
  prepend(batch) {
    const fresh = batch.messages.filter((m) => !bubbles.has(m.uid)).sort(byDate);
    loadingOlder = false;
    setOlder(batch.hasOlder);
    if (!fresh.length) return;
    const before = scroller.scrollHeight;
    const top = scroller.scrollTop;
    const frag = build(fresh, null);
    // Первый разделитель старой части лишний, если день тот же.
    const firstSep = feed.querySelector('.ddm-day');
    const lastNew = fresh[fresh.length - 1];
    if (firstSep && firstSep.dataset.day === dayKey(lastNew.dateTs || 0)) firstSep.remove();
    feed.insertBefore(frag, feed.firstChild);
    order = fresh.concat(order);
    fresh.forEach((m) => fit(bubbles.get(m.uid)));
    scroller.scrollTop = top + (scroller.scrollHeight - before);
  },

  /** Новые письма снизу; был внизу — остаёмся внизу. */
  append(batch) {
    const fresh = batch.messages.filter((m) => !bubbles.has(m.uid)).sort(byDate);
    if (!fresh.length) return;
    const atBottom = scroller.scrollHeight - scroller.scrollTop - window.innerHeight < 80;
    const prev = order.length ? order[order.length - 1].dateTs : null;
    // Настоящее письмо пришло — его заглушка уходит (сверка по тексту).
    for (const m of fresh) {
      if (!m.outgoing) continue;
      const norm = normText(plainOf(m));
      for (const [key, st] of stubs) {
        if (!st.failed && st.norm && norm.startsWith(st.norm)) {
          ddm.unstub(key);
          break;
        }
      }
    }
    feed.appendChild(build(fresh, prev));
    // Заглушки — всегда последними.
    for (const st of stubs.values()) feed.appendChild(st.row);
    order = order.concat(fresh);
    fresh.forEach((m) => fit(bubbles.get(m.uid)));
    if (atBottom) scroller.scrollTop = scroller.scrollHeight;
  },

  /** Мгновенный пузырь отправляемого письма. */
  stub(st) {
    ddm.unstub(st.key);
    // Заглушка переехала в беседу, где письмо уже пришло, — не дублируем.
    const norm = normText(st.text);
    const now = Date.now() / 1000;
    if (norm && order.some((m) => m.outgoing && now - (m.dateTs || 0) < 600 && normText(plainOf(m)).startsWith(norm))) return;
    const row = makeStub(st);
    feed.appendChild(row);
    const entry = { row, norm: normText(st.text), failed: false };
    stubs.set(st.key, entry);
    // Против «призраков»: письмо так и не пришло — заглушка уходит сама
    // (кроме не ушедшей: её пометка «Не доставлено» должна остаться).
    setTimeout(() => {
      if (stubs.get(st.key) === entry && !entry.failed) ddm.unstub(st.key);
    }, STUB_TTL_MS);
    scroller.scrollTop = scroller.scrollHeight;
  },

  /** Статус доставки письма сменился — пометка в пузыре на месте, без перерисовки письма. */
  delivery(uid, d) {
    const e = bubbles.get(uid);
    if (!e) return;
    e.msg.delivery = d;
    keepBottom(() => renderDelivery(e.deliveryEl, e.row, d));
  },

  /** Статус заглушки (IMAP: SMTP отказал — `failed` с причиной). */
  stubDelivery(key, d) {
    const st = stubs.get(key);
    if (!st) return;
    keepBottom(() => stubDelivery(st, d));
  },

  /**
   * Письма удалены (меню пузыря «Удалить письмо» или на другом устройстве):
   * пузыри уходят, разделитель дня без писем — тоже.
   */
  remove(uids) {
    const gone = new Set((uids || []).map(Number));
    if (!gone.size) return;
    for (const uid of gone) {
      const e = bubbles.get(uid);
      if (!e) continue;
      const prev = e.row.previousElementSibling;
      const next = e.row.nextElementSibling;
      e.row.remove();
      bubbles.delete(uid);
      textViews.delete(uid);
      const lonely = prev && prev.classList.contains('ddm-day') &&
        (!next || next.classList.contains('ddm-day'));
      if (lonely) prev.remove();
    }
    order = order.filter((m) => !gone.has(Number(m.uid)));
  },

  /** «Показать… → Текстовую версию» (`text`) / «HTML-версию» (`html`) — у одного письма. */
  view(uid, mode) {
    const e = bubbles.get(Number(uid));
    if (!e) return;
    const text = mode === 'text';
    if (e.viewText === text) return;
    e.viewText = text;
    if (text) textViews.add(e.msg.uid); else textViews.delete(e.msg.uid);
    holdAnchor(e.row);
    fill(e);
    keepAnchor();
  },

  /** Отправка не удалась — заглушку убрать: не врём, что ушло. */
  unstub(key) {
    const s = stubs.get(key);
    if (!s) return;
    s.row.remove();
    stubs.delete(key);
  },

  /**
   * Текст выделения для «Скопировать» нативной части. WebKit для выделения
   * внутри shadow root отдаёт из `getSelection().toString()` пустую строку —
   * текст берётся из составного диапазона письма.
   */
  selectedText() {
    const r = selectionRange();
    const t = r ? r.toString() : '';
    return t || String(window.getSelection() || '');
  },

  /** Клавиатура открылась/закрылась (WKWebView, см. keyboardUp). */
  keyboard(on) {
    const was = keyboardUp;
    keyboardUp = !!on;
    if (!was || keyboardUp || !touchWebKit) return;
    // Первый тап по письму убрал клавиатуру, второй WebKit забрал себе и
    // выделил слово сам — без меню и без `selectionchange`. Секунду смотрим,
    // не появилось ли выделение, и просим к нему меню.
    let tries = 12;
    const poll = setInterval(() => {
      const r = selectionRange();
      if (r && !r.collapsed) {
        clearInterval(poll);
        const b = r.getBoundingClientRect();
        native('selectionMenu', b.left + scrollX, b.top + scrollY, b.width, b.height);
      } else if (--tries <= 0) clearInterval(poll);
    }, 100);
  },

  /** Прямоугольник выделения в CSS px документа `[x, y, w, h]` или null. */
  selectionRect() {
    const r = selectionRange();
    if (!r || r.collapsed) return null;
    const b = r.getBoundingClientRect();
    return [b.left + scrollX, b.top + scrollY, b.width, b.height];
  },

  version: NORMALIZER_VERSION,
};
window.ddm = ddm;

window.addEventListener('scroll', () => {
  if (hasOlder && !loadingOlder && scroller.scrollTop < 400) {
    loadingOlder = true;
    native('requestOlder');
  }
}, { passive: true });

// Сложили/разложили телефон — ширина другая, масштаб писем пересчитать.
let resizeTimer = 0;
window.addEventListener('resize', () => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(fitAll, 100);
});

native('ready');
