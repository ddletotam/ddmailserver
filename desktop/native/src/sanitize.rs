//! Pre-Ultralight HTML cleanup.
//!
//! The bundled Ultralight (a fork of WebKit 615.x) is more brittle than
//! modern browsers when it meets the kind of HTML you actually find in
//! mail bodies — `<script>`, `<iframe>`, MS-Outlook conditional
//! comments, inline `on*=` handlers, etc. Whole bubbles silently render
//! blank when we hit a rough edge, so we shake the worst of it out
//! before handing the markup to the renderer. Conservative: anything
//! we don't recognise is left alone, including `<style>` (Ultralight
//! still needs that for layout).
//!
//! This is a regex pass, not a real parser — Outlook-generated mail
//! routinely violates spec, and a strict parser would either bail or
//! mangle layout. Regexes are good enough for the targeted strip set
//! and stay fast on the hot path.

use regex::Regex;
use std::collections::BTreeSet;
use std::sync::OnceLock;

use crate::policy::Policy;

struct Strips {
    re_script: Regex,
    re_iframe: Regex,
    re_form: Regex,
    re_object: Regex,
    re_embed_self: Regex,
    re_base: Regex,
    re_meta_refresh: Regex,
    re_mso_conditional: Regex,
    re_on_handler_quoted: Regex,
    re_on_handler_unquoted: Regex,
}

fn strips() -> &'static Strips {
    static R: OnceLock<Strips> = OnceLock::new();
    R.get_or_init(|| Strips {
        re_script: Regex::new(r"(?is)<script\b[^>]*>.*?</script\s*>").unwrap(),
        re_iframe: Regex::new(r"(?is)<iframe\b[^>]*>.*?</iframe\s*>|<iframe\b[^>]*/?>").unwrap(),
        re_form: Regex::new(r"(?is)<form\b[^>]*>.*?</form\s*>").unwrap(),
        re_object: Regex::new(r"(?is)<object\b[^>]*>.*?</object\s*>").unwrap(),
        re_embed_self: Regex::new(r"(?is)<embed\b[^>]*/?>").unwrap(),
        re_base: Regex::new(r"(?is)<base\b[^>]*/?>").unwrap(),
        re_meta_refresh: Regex::new(r#"(?is)<meta\b[^>]*http-equiv\s*=\s*["']?refresh["']?[^>]*/?>"#).unwrap(),
        // Outlook conditional comments wrap whole alternative trees and
        // contain MS-only markup Ultralight chokes on. Drop the whole
        // block.
        re_mso_conditional: Regex::new(r"(?is)<!--\s*\[if\s+[^\]]*\]>.*?<!\s*\[endif\]\s*-->").unwrap(),
        // Inline event handlers — non-functional anyway since we don't
        // run JS, but parsing them slows the layout and occasionally
        // confuses the attribute scanner.
        re_on_handler_quoted: Regex::new(r#"(?i)\bon[a-z]+\s*=\s*("[^"]*"|'[^']*')"#).unwrap(),
        re_on_handler_unquoted: Regex::new(r"(?i)\bon[a-z]+\s*=\s*[^\s>]+").unwrap(),
    })
}

/// Apply the strip passes. Empty in → empty out.
pub fn sanitize_email_html(input: &str) -> String {
    if input.is_empty() {
        return String::new();
    }
    let s = strips();
    let mut out = input.to_string();
    out = s.re_mso_conditional.replace_all(&out, "").into_owned();
    out = s.re_script.replace_all(&out, "").into_owned();
    out = s.re_iframe.replace_all(&out, "").into_owned();
    out = s.re_form.replace_all(&out, "").into_owned();
    out = s.re_object.replace_all(&out, "").into_owned();
    out = s.re_embed_self.replace_all(&out, "").into_owned();
    out = s.re_base.replace_all(&out, "").into_owned();
    out = s.re_meta_refresh.replace_all(&out, "").into_owned();
    out = s.re_on_handler_quoted.replace_all(&out, "").into_owned();
    out = s.re_on_handler_unquoted.replace_all(&out, "").into_owned();
    // Note: we intentionally do NOT strip <head>/<style>/<html>/<body> —
    // the email's own <style> block is what holds its layout together,
    // and WebKit tolerates the nested-doctype shape we end up with.
    out
}

/// Result of running the external-content blocker over a message body.
pub struct BlockOutcome {
    pub html: String,
    /// Domains that we replaced/blocked, sorted, lowercased. The UI
    /// uses this to populate the "allow per-domain" submenu.
    pub blocked_domains: Vec<String>,
}

struct BlockRes {
    re_img: Regex,
    re_link_remote: Regex,
    re_inline_url: Regex,
    re_bg_attr: Regex,
}

fn block_res() -> &'static BlockRes {
    static R: OnceLock<BlockRes> = OnceLock::new();
    R.get_or_init(|| BlockRes {
        // src= on media/iframe-ish tags (iframe already removed by sanitize,
        // but we keep it here for safety).
        re_img: Regex::new(
            r#"(?is)<(img|video|audio|source|iframe|embed)\b([^>]*?)\bsrc\s*=\s*("([^"]*)"|'([^']*)')"#,
        )
        .unwrap(),
        // External CSS stylesheets via <link>.
        re_link_remote: Regex::new(
            r#"(?is)<link\b[^>]*?href\s*=\s*("([^"]*)"|'([^']*)')[^>]*?>"#,
        )
        .unwrap(),
        // url(...) inside inline style attributes / <style> bodies.
        re_inline_url: Regex::new(
            r#"(?is)url\(\s*("([^"]*)"|'([^']*)'|([^)'"\s]+))\s*\)"#,
        )
        .unwrap(),
        // Old-school background="https://..." attribute.
        re_bg_attr: Regex::new(
            r#"(?is)\bbackground\s*=\s*("([^"]*)"|'([^']*)')"#,
        )
        .unwrap(),
    })
}

fn extract_host(url: &str) -> Option<String> {
    let trimmed = url.trim();
    if trimmed.is_empty() || trimmed.starts_with("data:") || trimmed.starts_with("cid:") || trimmed.starts_with('#') {
        return None;
    }
    let lower = trimmed.to_lowercase();
    let scheme_end = if lower.starts_with("http://") {
        7
    } else if lower.starts_with("https://") {
        8
    } else if lower.starts_with("//") {
        2
    } else {
        return None;
    };
    let rest = &trimmed[scheme_end..];
    let end = rest
        .find(|c: char| c == '/' || c == '?' || c == '#')
        .unwrap_or(rest.len());
    let host = &rest[..end];
    if host.is_empty() {
        None
    } else {
        Some(host.to_lowercase())
    }
}

/// Strip / blank out external resource URLs unless the sender is
/// trusted (per `policy.media_allowed`) or the resource's host is
/// already on the allow-list. Pure HTML regex pass — runs on the
/// post-sanitized body before WebKit sees it.
///
/// Returns the rewritten HTML plus the set of domains we blocked, so
/// the UI can offer per-domain "allow" toggles.
pub fn block_external(input: &str, policy: &Policy, sender: &str) -> BlockOutcome {
    if input.is_empty() {
        return BlockOutcome {
            html: String::new(),
            blocked_domains: Vec::new(),
        };
    }
    if policy.media_allowed(sender) {
        // Sender-trusted: nothing to block. Domain-allow list still
        // applies to scripts elsewhere but doesn't change <img>/url().
        return BlockOutcome {
            html: input.to_string(),
            blocked_domains: Vec::new(),
        };
    }

    let res = block_res();
    let mut blocked: BTreeSet<String> = BTreeSet::new();

    let mut out = res
        .re_img
        .replace_all(input, |caps: &regex::Captures| {
            let tag = &caps[1];
            let attrs_before_src = &caps[2];
            let url = caps.get(4).or_else(|| caps.get(5)).map(|m| m.as_str()).unwrap_or("");
            match extract_host(url) {
                Some(host) if !policy.domain_allowed(&host) => {
                    blocked.insert(host);
                    format!(
                        r#"<{tag}{attrs_before_src} data-blocked-src="{}" src="""#,
                        url.replace('"', "&quot;")
                    )
                }
                _ => caps[0].to_string(),
            }
        })
        .into_owned();

    out = res
        .re_link_remote
        .replace_all(&out, |caps: &regex::Captures| {
            let url = caps.get(2).or_else(|| caps.get(3)).map(|m| m.as_str()).unwrap_or("");
            match extract_host(url) {
                Some(host) if !policy.domain_allowed(&host) => {
                    blocked.insert(host);
                    String::new() // drop the whole <link>
                }
                _ => caps[0].to_string(),
            }
        })
        .into_owned();

    out = res
        .re_bg_attr
        .replace_all(&out, |caps: &regex::Captures| {
            let url = caps.get(2).or_else(|| caps.get(3)).map(|m| m.as_str()).unwrap_or("");
            match extract_host(url) {
                Some(host) if !policy.domain_allowed(&host) => {
                    blocked.insert(host);
                    String::new()
                }
                _ => caps[0].to_string(),
            }
        })
        .into_owned();

    out = res
        .re_inline_url
        .replace_all(&out, |caps: &regex::Captures| {
            let url = caps
                .get(2)
                .or_else(|| caps.get(3))
                .or_else(|| caps.get(4))
                .map(|m| m.as_str())
                .unwrap_or("");
            match extract_host(url) {
                Some(host) if !policy.domain_allowed(&host) => {
                    blocked.insert(host);
                    "url()".to_string()
                }
                _ => caps[0].to_string(),
            }
        })
        .into_owned();

    BlockOutcome {
        html: out,
        blocked_domains: blocked.into_iter().collect(),
    }
}
