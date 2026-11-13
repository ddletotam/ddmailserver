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
//   allowSender(uid), allowSpamImages(uid)
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
 * Долгое нажатие → меню. Android WebView и Мак (правый клик) присылают
 * `contextmenu`; WKWebView на iPhone/iPad при долгом нажатии его не шлёт, а
 * начинает выделение текста — меню пузыря и вложения были недостижимы.
 * Поэтому на сенсорном WebKit долгое нажатие считаем сами: 450 мс без
 * сдвига пальца дальше 10 px, затем гасим начатое выделение и следующий клик.
 */
const touchWebKit = !window.ddmNative && !!(window.webkit && window.webkit.messageHandlers) &&
  ('ontouchstart' in window);
function onLongPress(el, handler) {
  el.addEventListener('contextmenu', (e) => { e.preventDefault(); e.stopPropagation(); handler(); });
  if (!touchWebKit) return;
  let timer = null, x = 0, y = 0, fired = false;
  const cancel = () => { if (timer) { clearTimeout(timer); timer = null; } };
  el.addEventListener('touchstart', (e) => {
    if (e.touches.length !== 1) { cancel(); return; }
    e.stopPropagation();
    fired = false;
    x = e.touches[0].clientX; y = e.touches[0].clientY;
    cancel();
    timer = setTimeout(() => {
      timer = null; fired = true;
      const sel = window.getSelection && window.getSelection();
      if (sel) sel.removeAllRanges();
      handler();
    }, 450);
  }, { passive: true });
  el.addEventListener('touchmove', (e) => {
    const t = e.touches[0];
    if (t && Math.hypot(t.clientX - x, t.clientY - y) > 10) cancel();
  }, { passive: true });
  el.addEventListener('touchend', cancel, { passive: true });
  el.addEventListener('touchcancel', cancel, { passive: true });
  // Палец отпущен после срабатывания — клик не должен открыть вложение/ссылку.
  el.addEventListener('click', (e) => { if (fired) { fired = false; e.preventDefault(); e.stopPropagation(); } }, true);
}
if (touchWebKit) document.documentElement.classList.add('ddm-touch-webkit');

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
  if (msg.html) {
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
      // Долгое нажатие на чип — «Открыть» / «Сохранить в Загрузки».
      onLongPress(chip, () => native('attachmentMenu', msg.uid, a.index));
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

  // Долгое нажатие на пузырь — нативное меню (ответить, переслать, …).
  onLongPress(row, () => native('bubbleMenu', msg.uid));

  const entry = { msg, host, shadow, root: null, imagesAllowed: !!msg.imagesAllowed, imagesBtn, imagesBox, row, deliveryEl };
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

  /** Отправка не удалась — заглушку убрать: не врём, что ушло. */
  unstub(key) {
    const s = stubs.get(key);
    if (!s) return;
    s.row.remove();
    stubs.delete(key);
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
