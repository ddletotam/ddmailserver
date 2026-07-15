//! Minimal read-only CardDAV client so the desktop can list/search contacts
//! against a PLAIN server (not just our native backend). Pairs with the
//! server's addressbook-query support (Phase 3).
//!
//! Scope (slice 1 of Phase 4): a direct addressbook-collection URL — no
//! principal autodiscovery yet — plus an addressbook-query REPORT. Enough for
//! our own CardDAV face and simple servers; full discovery and foreign-server
//! quirks (Yandex/iCloud) are later refinements. Basic auth, read-only.

use crate::types::DesktopContact;

/// Fetch contacts from an addressbook collection URL. `query = None` (or empty)
/// lists everything; a non-empty query is a name/email substring search.
pub async fn fetch_contacts(
    addressbook_url: &str,
    username: &str,
    password: &str,
    query: Option<&str>,
    limit: usize,
) -> Result<Vec<DesktopContact>, String> {
    let filter = match query {
        Some(q) if !q.trim().is_empty() => format!(
            r#"  <C:filter test="anyof">
    <C:prop-filter name="FN"><C:text-match match-type="contains">{q}</C:text-match></C:prop-filter>
    <C:prop-filter name="EMAIL"><C:text-match match-type="contains">{q}</C:text-match></C:prop-filter>
  </C:filter>
"#,
            q = xml_escape(q.trim())
        ),
        _ => String::new(),
    };
    let body = format!(
        r#"<?xml version="1.0" encoding="utf-8"?>
<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop><D:getetag/><C:address-data/></D:prop>
{filter}</C:addressbook-query>"#
    );

    let http = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(20))
        .build()
        .map_err(|e| format!("http client: {e}"))?;
    let method = reqwest::Method::from_bytes(b"REPORT").map_err(|e| format!("method: {e}"))?;
    let resp = http
        .request(method, addressbook_url)
        .basic_auth(username, Some(password))
        .header("Depth", "1")
        .header(reqwest::header::CONTENT_TYPE, "application/xml; charset=utf-8")
        .body(body)
        .send()
        .await
        .map_err(|e| format!("carddav REPORT: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!("carddav REPORT HTTP {}", resp.status()));
    }
    let xml = resp.text().await.map_err(|e| format!("carddav body: {e}"))?;

    let mut out = Vec::new();
    for card in extract_address_data(&xml) {
        if let Some(c) = parse_vcard(&card) {
            out.push(c);
            if out.len() >= limit {
                break;
            }
        }
    }
    Ok(out)
}

/// Pull each `<…:address-data>…</…:address-data>` payload out of a DAV
/// multistatus, namespace-prefix agnostic, and XML-unescape it. Hand-rolled
/// (no XML dep); good enough for well-formed multistatus bodies.
fn extract_address_data(xml: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut rest = xml;
    while let Some(open_rel) = rest.find("address-data") {
        // Skip if this hit is a CLOSING tag ("</…address-data").
        let before = &rest[..open_rel];
        let is_close = before.trim_end().ends_with("</")
            || before.trim_end().ends_with('/'); // "</C:" style
        // Advance to the end of the start tag.
        let after_name = &rest[open_rel + "address-data".len()..];
        let Some(gt) = after_name.find('>') else { break };
        // Self-closing `<C:address-data/>` (the <D:prop> template) — skip it.
        let tag_inner = &after_name[..gt];
        if is_close || tag_inner.trim_end().ends_with('/') {
            rest = &after_name[gt + 1..];
            continue;
        }
        let content_start = open_rel + "address-data".len() + gt + 1;
        let body_region = &rest[content_start..];
        let Some(close_rel) = body_region.find("address-data>") else { break };
        // Content runs up to the "</PREFIX:" that precedes the closing tag.
        let raw = &body_region[..close_rel];
        let content = raw.rsplit_once('<').map(|(a, _)| a).unwrap_or(raw);
        let trimmed = content.trim();
        if !trimmed.is_empty() {
            out.push(xml_unescape(trimmed));
        }
        rest = &body_region[close_rel + "address-data>".len()..];
    }
    out
}

/// Parse the handful of vCard properties we surface. Handles line folding and
/// `PROP;PARAMS:value`. Returns None if there's no usable identity at all.
fn parse_vcard(vcard: &str) -> Option<DesktopContact> {
    let unfolded = unfold_vcard(vcard);
    let mut full_name = String::new();
    let mut emails = Vec::new();
    let mut phones = Vec::new();
    let mut org = String::new();
    let mut title = String::new();
    for line in unfolded.lines() {
        let Some((head, value)) = line.split_once(':') else { continue };
        let value = value.trim();
        if value.is_empty() {
            continue;
        }
        let name = head.split(';').next().unwrap_or("").to_ascii_uppercase();
        match name.as_str() {
            "FN" => full_name = value.to_string(),
            "EMAIL" => emails.push(value.to_string()),
            "TEL" => phones.push(value.to_string()),
            // ORG is ";"-structured (Company;Dept); take the first component.
            "ORG" => org = value.split(';').next().unwrap_or(value).trim().to_string(),
            "TITLE" => title = value.to_string(),
            _ => {}
        }
    }
    if full_name.is_empty() && emails.is_empty() {
        return None;
    }
    Some(DesktopContact {
        id: 0,
        full_name,
        emails,
        phones,
        organization: org,
        title,
        photo_url: String::new(),
    })
}

/// vCard line unfolding: a line beginning with a space/tab continues the
/// previous one (RFC 6350 §3.2).
fn unfold_vcard(vcard: &str) -> String {
    let mut out = String::new();
    for line in vcard.replace("\r\n", "\n").lines() {
        if line.starts_with(' ') || line.starts_with('\t') {
            out.push_str(line.trim_start());
        } else {
            if !out.is_empty() {
                out.push('\n');
            }
            out.push_str(line);
        }
    }
    out
}

fn xml_escape(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
}

fn xml_unescape(s: &str) -> String {
    s.replace("&lt;", "<")
        .replace("&gt;", ">")
        .replace("&quot;", "\"")
        .replace("&#39;", "'")
        .replace("&apos;", "'")
        .replace("&amp;", "&")
}
