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

function blockedPlaceholder(originalUrl: string): string {
  const domain = extractDomain(originalUrl);
  const label = domain || "external";
  return `<span data-blocked-src="${originalUrl.replace(/"/g, "&quot;")}" `
    + `style="display:inline-block;background:#e8e8e8;color:#888;border:1px dashed #ccc;`
    + `border-radius:4px;padding:6px 10px;margin:2px 0;font-size:12px;cursor:pointer;`
    + `font-family:sans-serif;" title="Click to load">`
    + `&#128274; ${label}</span>`;
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

  // Replace blocked <img> with placeholders
  result = result.replace(
    /<img\b[^>]*?data-blocked-src\s*=\s*["']([^"']*?)["'][^>]*?\/?>/gi,
    (_match, url) => blockedPlaceholder(url)
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
