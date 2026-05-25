// ── Plain text cleanup ──

/** Remove URLs from text that are hidden behind labels in HTML */
function removeHiddenUrls(text: string, html: string): string {
  const hiddenPrefixes: string[] = [];
  const linkRe = /<a[^>]*href=["']([^"']*)["'][^>]*>([\s\S]*?)<\/a>/gi;
  let m;
  while ((m = linkRe.exec(html)) !== null) {
    const href = m[1].replace(/&amp;/g, "&");
    const linkText = m[2].replace(/<[^>]*>/g, "").trim();
    if (linkText !== href && !linkText.startsWith("http")) {
      try {
        const u = new URL(href);
        hiddenPrefixes.push(u.origin + u.pathname);
      } catch {
        if (href.length > 20) hiddenPrefixes.push(href.substring(0, 40));
      }
    }
  }
  if (hiddenPrefixes.length === 0) return text;

  let result = text;
  for (const prefix of hiddenPrefixes) {
    const escaped = prefix.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    result = result.replace(new RegExp(`${escaped}[^\\s]*`, "g"), "");
  }
  result = result.replace(/\n\s*\n\s*\n/g, "\n\n");
  return result.trim();
}

/** Clean up plain text for display */
export function cleanPlainText(text: string, html: string | null): string {
  let s = text;
  s = s.replace(/!\[[^\]]*\]\([^)]*\)/g, "");
  s = s.replace(/\[([^\]]*)\]\([^)]*\)/g, "$1");
  if (html) s = removeHiddenUrls(s, html);
  s = s.replace(/^\s*---+\s*$/gm, "");
  s = s.replace(/^\s*\|\s*$/gm, "");
  s = s.replace(/^\s*\]\s*$/gm, "");
  s = s.replace(/\n{3,}/g, "\n\n");
  return s.trim();
}

// ── Permission state passed as plain data (no store import) ──

export interface ContentPermissions {
  mediaAllowed: boolean;
  scriptsAllowed: boolean;
  allowedDomains: string[];
}

// ── iframe HTML preparation ──

function isExternalUrl(url: string): boolean {
  const trimmed = url.trim().toLowerCase();
  if (!trimmed || trimmed.startsWith("data:") || trimmed.startsWith("cid:") || trimmed.startsWith("#")) {
    return false;
  }
  return trimmed.startsWith("http:") || trimmed.startsWith("https:") || trimmed.startsWith("//");
}

function extractDomain(url: string): string {
  try {
    return new URL(url, "https://placeholder.invalid").hostname;
  } catch {
    return "";
  }
}

function blockedPlaceholder(originalUrl: string, width?: string, height?: string, alt?: string): string {
  const w = width || "";
  const h = height || "";
  const styleW = w ? `width:${w};` : "";
  const styleH = h ? `height:${h};` : "";
  const minStyle = (!w && !h) ? "min-width:8px;min-height:8px;" : "";
  const label = alt ? alt.replace(/</g, "&lt;").replace(/>/g, "&gt;") : "";
  return `<span data-blocked-src="${originalUrl.replace(/"/g, "&quot;")}" `
    + `style="display:inline-block;${styleW}${styleH}${minStyle}max-width:100%;`
    + `background:#eee;border-radius:2px;cursor:pointer;vertical-align:middle;`
    + `font-size:11px;color:#999;overflow:hidden;" title="Click to load">`
    + `${label}</span>`;
}

function isDomainInList(domain: string, allowedDomains: string[]): boolean {
  const lower = domain.toLowerCase();
  return allowedDomains.some(d => d === lower);
}

/** Block external resources in HTML */
function blockExternalResources(html: string, allowedDomains: string[]): string {
  let result = html;

  // Block src= on media elements
  result = result.replace(
    /(<(?:img|video|audio|source|iframe|embed)\b[^>]*?\b)(src\s*=\s*)(["'])([^"']*?)\3/gi,
    (match, before, attr, quote, url) => {
      if (!isExternalUrl(url)) return match;
      if (isDomainInList(extractDomain(url), allowedDomains)) return match;
      return `${before}data-blocked-src=${quote}${url}${quote} src=${quote}${quote}`;
    }
  );

  // Replace blocked <img> with placeholders preserving dimensions and alt
  result = result.replace(
    /<img\b([^>]*?)data-blocked-src\s*=\s*["']([^"']*?)["']([^>]*?)\/?>/gi,
    (_match, before, url, after) => {
      const allAttrs = before + after;
      const wMatch = allAttrs.match(/\bwidth\s*=\s*["']?(\d+)/i);
      const hMatch = allAttrs.match(/\bheight\s*=\s*["']?(\d+)/i);
      const altMatch = allAttrs.match(/\balt\s*=\s*["']([^"']*?)["']/i);
      const w = wMatch ? wMatch[1] + "px" : undefined;
      const h = hMatch ? hMatch[1] + "px" : undefined;
      const alt = altMatch ? altMatch[1] : undefined;
      return blockedPlaceholder(url, w, h, alt);
    }
  );

  // Block background= attributes
  result = result.replace(
    /(\bbackground\s*=\s*)(["'])([^"']*?)\2/gi,
    (match, attr, quote, url) => {
      if (!isExternalUrl(url)) return match;
      if (isDomainInList(extractDomain(url), allowedDomains)) return match;
      return `data-blocked-bg=${quote}${url}${quote}`;
    }
  );

  // Block url() in inline styles
  result = result.replace(
    /(style\s*=\s*["'][^"']*?)url\(\s*["']?(https?:\/\/[^"')]+)["']?\s*\)/gi,
    (match, before, url) => {
      if (isDomainInList(extractDomain(url), allowedDomains)) return match;
      return `${before}url()`;
    }
  );

  // Block <link> stylesheets
  result = result.replace(
    /<link\b[^>]*?href\s*=\s*["'](https?:\/\/[^"']*?)["'][^>]*?\/?>/gi,
    (match, url) => {
      if (isDomainInList(extractDomain(url), allowedDomains)) return match;
      return `<!-- blocked: ${extractDomain(url)} -->`;
    }
  );

  return result;
}

/** Prepare HTML fragment for shadow DOM rendering */
export function prepareEmailHtml(
  html: string,
  permissions: ContentPermissions,
  isDark: boolean,
): string {
  let processedHtml = html;

  // Strip scripts unless allowed
  if (!permissions.scriptsAllowed) {
    const scrTag = "scr" + "ipt";
    processedHtml = processedHtml.replace(
      new RegExp(`<${scrTag}[\\s\\S]*?<\\/${scrTag}>`, "gi"), ""
    );
    // Strip inline event handlers (on*= attributes)
    processedHtml = processedHtml.replace(/\bon\w+\s*=\s*["'][^"']*["']/gi, "");
    processedHtml = processedHtml.replace(/\bon\w+\s*=\s*[^\s>]*/gi, "");
    // Strip javascript: and data:text/html URLs in href/src/action
    processedHtml = processedHtml.replace(/\b(href|src|action)\s*=\s*["']\s*javascript:[^"']*["']/gi, '$1=""');
    processedHtml = processedHtml.replace(/\b(href|src|action)\s*=\s*["']\s*data:text\/html[^"']*["']/gi, '$1=""');
    // Strip <meta http-equiv="refresh">
    processedHtml = processedHtml.replace(/<meta[^>]*?http-equiv\s*=\s*["']refresh["'][^>]*?\/?>/gi, "");
    // Strip <base> tags (can redirect all relative URLs)
    processedHtml = processedHtml.replace(/<base\b[^>]*?\/?>/gi, "");
    // Strip <form> tags (phishing vector)
    processedHtml = processedHtml.replace(/<form\b[^>]*?>[\s\S]*?<\/form>/gi, "");
    processedHtml = processedHtml.replace(/<form\b[^>]*?\/?>/gi, "");
  }

  // Strip existing <style> tags that could leak out (shadow DOM isolates, but be safe)
  // Keep them — shadow DOM isolates styles properly

  // Block external resources if media not allowed
  if (!permissions.mediaAllowed) {
    processedHtml = blockExternalResources(processedHtml, permissions.allowedDomains);
  }

  const darkStyles = isDark
    ? `:host { color: #e0e0e0; } a { color: #6ab3f3 !important; }`
    : `:host { color: #1a1a1a; }`;

  const constraintStyles = `
    :host {
      display: block;
      font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
      font-size: 14px; line-height: 1.5;
      max-width: 100%; overflow-x: hidden;
      word-wrap: break-word; overflow-wrap: break-word;
    }
    img { max-width: 100% !important; height: auto !important; }
    table { max-width: 100% !important; }
    td, th { word-wrap: break-word; overflow-wrap: break-word; }
    pre { overflow-x: auto; max-width: 100%; }
    blockquote { border-left: 3px solid #ccc; margin: 8px 0; padding: 4px 12px; color: #666; }
    a { color: #3390ec; }
    * { max-width: 100% !important; box-sizing: border-box; }
  `;

  return `<style>${constraintStyles}${darkStyles}</style>${processedHtml}`;
}

/** Extract unique external domains from HTML that would be blocked */
export function extractBlockedDomains(html: string): string[] {
  const domains = new Set<string>();

  // src= on media elements
  const srcRe = /<(?:img|video|audio|source|iframe|embed)\b[^>]*?\bsrc\s*=\s*["'](https?:\/\/[^"']*?)["']/gi;
  let m;
  while ((m = srcRe.exec(html)) !== null) {
    const d = extractDomain(m[1]);
    if (d) domains.add(d);
  }

  // background= attributes
  const bgRe = /\bbackground\s*=\s*["'](https?:\/\/[^"']*?)["']/gi;
  while ((m = bgRe.exec(html)) !== null) {
    const d = extractDomain(m[1]);
    if (d) domains.add(d);
  }

  // url() in inline styles
  const urlRe = /url\(\s*["']?(https?:\/\/[^"')]+)["']?\s*\)/gi;
  while ((m = urlRe.exec(html)) !== null) {
    const d = extractDomain(m[1]);
    if (d) domains.add(d);
  }

  // <link href="...">
  const linkRe = /<link\b[^>]*?href\s*=\s*["'](https?:\/\/[^"']*?)["']/gi;
  while ((m = linkRe.exec(html)) !== null) {
    const d = extractDomain(m[1]);
    if (d) domains.add(d);
  }

  return [...domains].sort();
}

/** Categorized domains found in email HTML */
export interface EmailDomains {
  imageDomains: string[];
  scriptDomains: string[];
  allDomains: string[];
}

/** Extract unique external domains from HTML, categorized by type */
export function extractEmailDomains(html: string): EmailDomains {
  const imageDomains = new Set<string>();
  const scriptDomains = new Set<string>();
  let m;

  const mediaSrcRe = /<(?:img|video|audio|source|iframe|embed)\b[^>]*?\bsrc\s*=\s*["'](https?:\/\/[^"']*?)["']/gi;
  while ((m = mediaSrcRe.exec(html)) !== null) {
    const d = extractDomain(m[1]);
    if (d) imageDomains.add(d);
  }
  const bgRe = /\bbackground\s*=\s*["'](https?:\/\/[^"']*?)["']/gi;
  while ((m = bgRe.exec(html)) !== null) {
    const d = extractDomain(m[1]);
    if (d) imageDomains.add(d);
  }
  const urlRe = /url\(\s*["']?(https?:\/\/[^"')]+)["']?\s*\)/gi;
  while ((m = urlRe.exec(html)) !== null) {
    const d = extractDomain(m[1]);
    if (d) imageDomains.add(d);
  }
  const linkRe = /<link\b[^>]*?href\s*=\s*["'](https?:\/\/[^"']*?)["']/gi;
  while ((m = linkRe.exec(html)) !== null) {
    const d = extractDomain(m[1]);
    if (d) imageDomains.add(d);
  }
  const scriptRe = /<script\b[^>]*?\bsrc\s*=\s*["'](https?:\/\/[^"']*?)["']/gi;
  while ((m = scriptRe.exec(html)) !== null) {
    const d = extractDomain(m[1]);
    if (d) scriptDomains.add(d);
  }

  return {
    imageDomains: [...imageDomains].sort(),
    scriptDomains: [...scriptDomains].sort(),
    allDomains: [...new Set([...imageDomains, ...scriptDomains])].sort(),
  };
}

// ── Quoted-history stripping ──
//
// Reply emails carry the previous correspondence as a quoted block inside
// the body. In a chat-bubble UI that's redundant — the prior messages are
// already their own bubbles. We try to detect the quote boundary and cut
// from there to the end, leaving just the new reply. If the result looks
// suspiciously empty (whole body was quotation, or detection misfired) the
// caller falls back to the original.

const QUOTE_CONTAINER_SELECTORS = [
  "blockquote",
  // Gmail
  ".gmail_quote", ".gmail_attr",
  // Yahoo
  ".yahoo_quoted",
  // Outlook desktop / OWA / Outlook 365
  ".OutlookMessageHeader",
  "[id^='divRpl']",
  "[id^='x_divRpl']",
  // Yandex Mail ("Предыдущая переписка")
  ".ya-q-block", ".js-helper-bar", "[data-zone-name='lastMessage']",
  // Planfix CRM injection (ships "Предыдущая переписка" + avatar bubbles)
  ".planfix-last-actions",
  // ProtonMail
  ".protonmail_quote",
  // Apple Mail (signature/attribution lines wrap in this on some versions)
  ".AppleMailSignature",
  // Generic catch-all by class name — :has() left out because WebKitGTK
  // support is uneven and a throw from querySelector would skip the whole
  // strip pass.
  "[class*='quote' i]",
];

const QUOTE_TEXT_HEADERS = [
  // English: "On <date>, <name> wrote:"
  /^[\s>]*on\b.*\bwrote:?\s*$/i,
  // English: "From: <addr>" Outlook-style attribution
  /^[\s>]*from:\s*.+/i,
  // Russian: "В <date> <name> писал(а):"
  /^[\s>]*в\s+.+(писал[а]?|написал[а]?):?\s*$/i,
  // Russian Yandex marker we've seen
  /^[\s>]*предыдущая переписка\s*$/i,
  // "------ Original Message ------" / "--- Пересланное сообщение ---"
  /^-{2,}\s*(original message|forwarded message|пересланное сообщение|исходное сообщение)\s*-{2,}\s*$/i,
];

/** Strip quoted history from an HTML email body. Returns the stripped HTML
 *  AND a boolean indicating whether anything was actually cut. */
export function stripQuotedHTML(html: string): { stripped: string; cut: boolean } {
  if (!html) return { stripped: html, cut: false };
  let doc: Document;
  try {
    doc = new DOMParser().parseFromString(html, "text/html");
  } catch {
    return { stripped: html, cut: false };
  }
  if (!doc.body) return { stripped: html, cut: false };

  let cut = false;

  // Phase 1: known quote containers — remove the element AND everything
  // after it in document order. The "everything after" is the important
  // part: clients often put the attribution line ("On <date> wrote:")
  // BEFORE the blockquote as a separate <p>, and we want that gone too.
  for (const sel of QUOTE_CONTAINER_SELECTORS) {
    let el: Element | null = null;
    try { el = doc.body.querySelector(sel); } catch { continue; }
    if (!el) continue;
    removeFromElementOnward(el);
    cut = true;
    break;
  }

  // Phase 2: text-based attribution headers, walked top-to-bottom across
  // text nodes. If we find one, drop everything from its parent onward.
  if (!cut) {
    const walker = doc.createTreeWalker(doc.body, NodeFilter.SHOW_TEXT);
    let node: Node | null;
    while ((node = walker.nextNode())) {
      const t = (node.nodeValue ?? "").trim();
      if (!t) continue;
      if (QUOTE_TEXT_HEADERS.some((re) => re.test(t))) {
        // Find the topmost block-level ancestor inside body so we drop the
        // whole attribution line, not just the text node.
        let anchor: Element | null = node.parentElement;
        while (anchor && anchor.parentElement && anchor.parentElement !== doc.body) {
          anchor = anchor.parentElement;
        }
        if (anchor) {
          removeFromElementOnward(anchor);
          cut = true;
        }
        break;
      }
    }
  }

  if (!cut) return { stripped: html, cut: false };
  return { stripped: doc.body.innerHTML, cut: true };
}

function removeFromElementOnward(el: Element) {
  let cursor: Element | null = el;
  while (cursor) {
    const next: Element | null = cursor.nextElementSibling;
    cursor.remove();
    cursor = next;
  }
}

/** Strip quoted history from a plain-text body. */
export function stripQuotedText(text: string): { stripped: string; cut: boolean } {
  if (!text) return { stripped: text, cut: false };
  const lines = text.split(/\r?\n/);
  let cutAt = -1;

  // First pass: contiguous block of `> ` lines that goes to the end.
  for (let i = 0; i < lines.length; i++) {
    if (/^\s{0,3}>/.test(lines[i])) {
      // Walk back over blank lines + the line just above (often the "On X wrote:" header).
      let start = i;
      let blanks = 0;
      while (start > 0 && lines[start - 1].trim() === "" && blanks < 2) {
        start--; blanks++;
      }
      if (start > 0 && QUOTE_TEXT_HEADERS.some((re) => re.test(lines[start - 1].trim()))) {
        start--;
      }
      cutAt = start;
      break;
    }
    if (QUOTE_TEXT_HEADERS.some((re) => re.test(lines[i].trim()))) {
      cutAt = i;
      break;
    }
  }

  if (cutAt < 0) return { stripped: text, cut: false };
  const kept = lines.slice(0, cutAt).join("\n").replace(/\s+$/g, "");
  return { stripped: kept, cut: kept !== text };
}

// Bail-out: when the stripped result is so short it's probably misdetection
// (the whole bubble would render essentially blank), the caller should fall
// back to the original. 30 chars is a soft threshold tuned to "barely a
// sentence" — covers "OK", emoji-only replies stay below, but anything
// substantive stays above.
export const MIN_USEFUL_STRIPPED_LEN = 30;

// ── Display content resolution ──

export type DisplayMode = "html" | "text" | "auto";

export function resolveDisplayContent(
  text: string | null,
  html: string | null,
  mode: DisplayMode = "auto",
): { type: "text" | "html" | "empty"; content: string } {
  if (mode === "text" && text && text.trim()) {
    return { type: "text", content: cleanPlainText(text, html) };
  }
  if (mode === "html" && html) {
    return { type: "html", content: html };
  }
  if (mode === "auto") {
    if (html) return { type: "html", content: html };
    if (text && text.trim()) return { type: "text", content: cleanPlainText(text, null) };
  }
  if (text && text.trim()) return { type: "text", content: cleanPlainText(text, html) };
  if (html) return { type: "html", content: html };
  return { type: "empty", content: "" };
}
