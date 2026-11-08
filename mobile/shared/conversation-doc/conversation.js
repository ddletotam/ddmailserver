// Лента пузырей открытой беседы (контракт Android §8). Общий для мобильных
// клиентов: нативный код только отдаёт письма и исполняет просьбы моста.
//
// Нативный → документ:  ddm.set(batch), ddm.prepend(batch), ddm.append(batch)
//   batch = { messages: [Msg], hasOlder: bool, focusUid?: number }
//   Msg   = { uid, outgoing, fromName, fromAddr, dateTs (сек), html?, text?,
//             attachments: [{index, filename, size}], showFrom }
// Документ → нативный:  ready(), openLink(url), requestOlder(),
//   openAttachment(uid, index)
'use strict';

/** Сменилась нормализация — поднять: клиенты пересчитают закэшированное. */
const NORMALIZER_VERSION = 1;

const native = (() => {
  if (window.ddmNative) {
    return (name, ...args) => window.ddmNative[name](...args);
  }
  const wk = window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.ddm;
  if (wk) return (name, ...args) => wk.postMessage({ name, args });
  return () => {};
})();

const feed = document.getElementById('feed');
const olderEl = document.getElementById('older');
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
function remoteUrl(u, imagesAllowed) {
  if (!imagesAllowed) return null;
  return '/remote?u=' + encodeURIComponent(u);
}

function rewriteCssUrls(css, uid, imagesAllowed) {
  return css.replace(/url\(\s*(['"]?)([^'")]+)\1\s*\)/gi, (all, q, u) => {
    const url = u.trim();
    if (/^cid:/i.test(url)) return `url("/cid/${uid}/${encodeURIComponent(url.slice(4))}")`;
    if (/^https?:/i.test(url)) {
      const r = remoteUrl(url, imagesAllowed);
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
function sanitize(html, uid, imagesAllowed) {
  // <base> вырезаем до разбора: иначе CSP документа (`base-uri 'none'`)
  // ругается на каждое такое письмо, хотя тег и так не дожил бы до вставки.
  const doc = new DOMParser().parseFromString(html.replace(/<base\b[^>]*>/gi, ''), 'text/html');
  const styles = [];
  doc.querySelectorAll('style').forEach((s) => {
    styles.push(rewriteCssUrls(tameCss(s.textContent || ''), uid, imagesAllowed));
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
      const r = remoteUrl(src, imagesAllowed);
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
    if (!text || !QUOTE_MARKERS.some((re) => re.test(text))) continue;
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
    const s = sanitize(msg.html, msg.uid, entry.imagesAllowed);
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
  entry.imagesBtn.hidden = !(remoteCount > 0 && !entry.imagesAllowed);
  if (!entry.imagesBtn.hidden) entry.imagesBtn.textContent = `Показать картинки (${remoteCount})`;
  link.addEventListener('load', () => fit(entry));
  root.querySelectorAll('img').forEach((img) => img.addEventListener('load', () => fit(entry)));
  if (entry.host.isConnected) fit(entry);
}

/**
 * Письмо шире пузыря — масштабируем целиком (`zoom`), а не раскладываем в
 * столбик: в линеаризованной таблице цифры теряют связь со столбцами (§8).
 */
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

  const imagesBtn = document.createElement('button');
  imagesBtn.className = 'ddm-images';
  imagesBtn.hidden = true;
  b.appendChild(imagesBtn);

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
      chip.append('📎', name, size);
      chip.addEventListener('click', () => native('openAttachment', msg.uid, a.index));
      box.appendChild(chip);
    }
    b.appendChild(box);
  }
  const meta = document.createElement('div');
  meta.className = 'ddm-meta';
  meta.textContent = msg.dateTs ? timeFmt.format(new Date(msg.dateTs * 1000)) : '';
  b.appendChild(meta);
  row.appendChild(b);

  const entry = { msg, host, shadow, root: null, imagesAllowed: false, imagesBtn };
  // Разрешение картинок — на это письмо и до закрытия беседы.
  imagesBtn.addEventListener('click', () => {
    entry.imagesAllowed = true;
    fill(entry);
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
  olderEl.textContent = 'Загружаем раньше…';
}

function fitAll() {
  for (const e of bubbles.values()) fit(e);
}

const ddm = {
  /** Новая беседа: всё заново. */
  set(batch) {
    bubbles.clear();
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
    feed.appendChild(build(fresh, prev));
    order = order.concat(fresh);
    fresh.forEach((m) => fit(bubbles.get(m.uid)));
    if (atBottom) scroller.scrollTop = scroller.scrollHeight;
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
