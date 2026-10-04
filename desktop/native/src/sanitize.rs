//! Regex cleanup of inbound mail HTML before it is wrapped into the bubble
//! document and handed to `emlrender`.
//!
//! What it is NOT: a security boundary for rendering. `emlrender` executes
//! no script, opens no socket and drops the active elements' subtrees on its
//! own (`dom::is_dropped`: `script`, `iframe`, `object`, `embed`, `form`,
//! `base`, `meta`, …); remote images reach the wire only through the
//! loader's per-host gate (`render.rs`, `Policy::media_gate`).
//!
//! What it is for:
//!   * MS-Outlook conditional blocks (`<!--[if …]>…<![endif]-->`) are
//!     removed wholesale — they carry Outlook-only alternative markup;
//!   * the «Медиа…» menu semantics for scripts (`sanitize_email_html_for`
//!     keeps them for a trusted sender / allowed host), and the
//!     `data-blocked-src` placeholders plus the per-host menu entries
//!     (`block_external`, `first_external_hosts`);
//!   * keeping the bubble document small and predictable: one DOM serves both
//!     our chrome and the sender's markup (contract §4б), and stripped
//!     subtrees cannot interact with our `ddm-` rules.
//! Conservative: anything we don't recognise is left alone, including
//! `<style>`, which holds the mail's own layout together.
//!
//! This is a regex pass, not a real parser — Outlook-generated mail
//! routinely violates spec, and a strict parser would either bail or
//! mangle layout. Regexes are good enough for the targeted strip set
//! and stay fast on the hot path.

use regex::Regex;
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
    re_mso_revealed_open: Regex,
    re_mso_revealed_close: Regex,
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
        re_meta_refresh: Regex::new(
            r#"(?is)<meta\b[^>]*http-equiv\s*=\s*["']?refresh["']?[^>]*/?>"#,
        )
        .unwrap(),
        // Outlook conditional comments wrap whole alternative trees of
        // MS-only markup (VML buttons, fixed-width ghost tables). Drop the
        // whole block — a browser sees it as one comment and hides it too.
        re_mso_conditional: Regex::new(r"(?is)<!--\s*\[if\s+[^\]]*\]>.*?<!\s*\[endif\]\s*-->")
            .unwrap(),
        // The "downlevel-revealed" form is the opposite: `<!--[if !mso]><!-->`
        // and `<!--<![endif]-->` are two self-closed comments, and what lies
        // between is the mail for every client *except* Outlook. Only the
        // markers go, and before the block pass, which would otherwise
        // swallow the content up to the closer (Steam and Gosuslugi lost
        // whole sections that way).
        re_mso_revealed_open: Regex::new(r"(?is)<!--\s*\[if\s+[^\]]*\]>\s*<!--(?:\s*--)?>").unwrap(),
        re_mso_revealed_close: Regex::new(r"(?is)<!--\s*<!\s*\[endif\]\s*-->").unwrap(),
        // Inline event handlers — non-functional anyway since we don't
        // run JS, but parsing them slows the layout and occasionally
        // confuses the attribute scanner.
        re_on_handler_quoted: Regex::new(r#"(?i)\bon[a-z]+\s*=\s*("[^"]*"|'[^']*')"#).unwrap(),
        re_on_handler_unquoted: Regex::new(r"(?i)\bon[a-z]+\s*=\s*[^\s>]+").unwrap(),
    })
}

/// Strip pass. Empty in → empty out. `<script>` and `on*=` handlers always
/// go: emlrender runs no JavaScript, so there is nothing to permit.
pub fn sanitize_email_html(input: &str) -> String {
    if input.is_empty() {
        return String::new();
    }
    let s = strips();
    let mut out = input.to_string();
    out = s.re_mso_revealed_open.replace_all(&out, "").into_owned();
    out = s.re_mso_revealed_close.replace_all(&out, "").into_owned();
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
    // the email's own <style> block is what holds its layout together, and
    // html5ever (emlrender's parser) folds the nested-document shape we end
    // up with inside the bubble template the same way a browser does.
    out
}

/// First external image host in the raw message HTML — feeds the per-host
/// item of the «Медиа…» menu. Empty when the message has no such resource.
pub fn first_external_host(html: &str) -> String {
    if html.is_empty() {
        return String::new();
    }
    let res = block_res();
    res
        .re_img
        .captures_iter(html)
        .filter_map(|c| {
            c.get(4)
                .or_else(|| c.get(5))
                .or_else(|| c.get(6))
                .and_then(|m| extract_host(m.as_str()))
        })
        .next()
        .unwrap_or_default()
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
        // but we keep it here for safety). Quoted or not: an unquoted `src`
        // the rewrite skipped would still be blocked by the loader's own gate
        // (`Policy::media_gate`), but its host would be missing from the
        // «Медиа…» menu, leaving the user nothing to click.
        re_img: Regex::new(
            r#"(?is)<(img|video|audio|source|iframe|embed)\b([^>]*?)\bsrc\s*=\s*("([^"]*)"|'([^']*)'|([^\s"'>]+))"#,
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
            r#"(?is)\bbackground\s*=\s*("([^"]*)"|'([^']*)'|([^\s"'>]+))"#,
        )
        .unwrap(),
    })
}

fn extract_host(url: &str) -> Option<String> {
    let trimmed = url.trim();
    if trimmed.is_empty()
        || trimmed.starts_with("data:")
        || trimmed.starts_with("cid:")
        || trimmed.starts_with('#')
    {
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
    let end = rest.find(|c: char| c == '/' || c == '?' || c == '#').unwrap_or(rest.len());
    let host = &rest[..end];
    if host.is_empty() { None } else { Some(host.to_lowercase()) }
}

/// Strip / blank out external resource URLs unless the sender is
/// trusted (per `policy.media_allowed`) or the resource's host is
/// already on the allow-list. Pure HTML regex pass — runs on the
/// post-sanitized body before `emlrender` sees it.
///
/// Presentation only: the URL survives in `data-blocked-src`, and the
/// «Медиа…» menu takes its hosts from `first_external_hosts`. Whatever a
/// regex here misses is still refused by the loader's gate.
pub fn block_external(input: &str, policy: &Policy, sender: &str) -> String {
    if input.is_empty() {
        return String::new();
    }
    if policy.media_allowed(sender) {
        // Sender-trusted: nothing to block. Domain-allow list still
        // applies to scripts elsewhere but doesn't change <img>/url().
        return input.to_string();
    }

    let res = block_res();

    let mut out = res
        .re_img
        .replace_all(input, |caps: &regex::Captures| {
            let tag = &caps[1];
            let attrs_before_src = &caps[2];
            let url = caps
                .get(4)
                .or_else(|| caps.get(5))
                .or_else(|| caps.get(6))
                .map(|m| m.as_str())
                .unwrap_or("");
            match extract_host(url) {
                Some(host) if !policy.domain_allowed(&host) => {
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
                    String::new() // drop the whole <link>
                }
                _ => caps[0].to_string(),
            }
        })
        .into_owned();

    out = res
        .re_bg_attr
        .replace_all(&out, |caps: &regex::Captures| {
            let url = caps
                .get(2)
                .or_else(|| caps.get(3))
                .or_else(|| caps.get(4))
                .map(|m| m.as_str())
                .unwrap_or("");
            match extract_host(url) {
                Some(host) if !policy.domain_allowed(&host) => String::new(),
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
                Some(host) if !policy.domain_allowed(&host) => "url()".to_string(),
                _ => caps[0].to_string(),
            }
        })
        .into_owned();

    out
}

#[cfg(test)]
mod tests {
    use super::*;

    /// `[if !mso]` content is the mail everyone but Outlook sees; the
    /// Outlook-only block next to it still goes.
    #[test]
    fn non_outlook_content_survives_outlook_blocks_go() {
        let html = r#"<p>до</p><!--[if mso]><table><tr><td>VML-кнопка</td></tr></table><![endif]-->
            <!--[if !mso]><!--><a href="https://x.example">Кнопка</a><!--<![endif]-->
            <!--[if !mso]><!-- --><b>ещё</b><!--<![endif]--><p>после</p>"#;
        let out = sanitize_email_html(html);
        assert!(out.contains("Кнопка</a>"), "{out}");
        assert!(out.contains("<b>ещё</b>"), "{out}");
        assert!(!out.contains("VML-кнопка"), "{out}");
        assert!(out.contains("до") && out.contains("после"), "{out}");
        assert!(!out.contains("[if") && !out.contains("endif"), "{out}");
    }

    #[test]
    fn unquoted_src_is_blocked_and_offered_in_the_menu() {
        let html = r#"<img src=https://t.example/p.gif><td background=https://b.example/bg.gif>"#;
        let out = block_external(html, &Policy::default(), "news@sender.example");
        assert!(!out.contains("src=https://"), "{out}");
        assert!(!out.contains("background=https://"), "{out}");
        assert!(out.contains(r#"data-blocked-src="https://t.example/p.gif""#), "{out}");
        assert_eq!(first_external_host(html), "t.example");
    }
}
