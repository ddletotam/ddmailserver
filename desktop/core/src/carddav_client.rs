//! Minimal read-only CardDAV client so the desktop can list/search contacts
//! against a PLAIN server (not just our native backend). Pairs with the
//! server's addressbook-query support (Phase 3).
//!
//! Scope (slice 1 of Phase 4): a direct addressbook-collection URL — no
//! principal autodiscovery yet — plus an addressbook-query REPORT. Enough for
//! our own CardDAV face and simple servers; full discovery and foreign-server
//! quirks (Yandex/iCloud) are later refinements. Basic auth, read-only.

use crate::caldav_client::DavAuthExt;
use crate::types::DesktopContact;

/// Resolve a configured CardDAV URL to a concrete addressbook-collection URL:
/// use it directly if it's already an addressbook, else walk
/// current-user-principal → addressbook-home-set → first addressbook (RFC
/// 6764). Falls back to the input if discovery finds nothing.
pub async fn resolve_addressbook_collection(
    url: &str,
    username: &str,
    password: &str,
) -> Result<String, String> {
    use crate::caldav_client::{abs_url, first_href, propfind, split_responses};
    let prop = r#"<D:prop><D:resourcetype/><D:current-user-principal/></D:prop>"#;
    let root = propfind(url, username, password, 0, prop).await?;
    if root.to_lowercase().contains("addressbook") {
        return Ok(url.to_string());
    }
    let principal = first_href(&root, "current-user-principal").unwrap_or_else(|| url.to_string());
    let principal = abs_url(url, &principal);
    let home_prop =
        r#"<D:prop><C:addressbook-home-set xmlns:C="urn:ietf:params:xml:ns:carddav"/></D:prop>"#;
    let home_xml = propfind(&principal, username, password, 0, home_prop).await?;
    let Some(home) = first_href(&home_xml, "addressbook-home-set") else {
        return Ok(url.to_string());
    };
    let home = abs_url(url, &home);
    let listing = propfind(&home, username, password, 1, prop).await?;
    for resp in split_responses(&listing) {
        if resp.to_lowercase().contains("addressbook")
            && resp.to_lowercase().contains("resourcetype")
        {
            if let Some(href) = first_href(&resp, "href") {
                return Ok(abs_url(&home, &href));
            }
        }
    }
    Ok(url.to_string())
}

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
        .dav_auth(username, password)
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

/// The contact resource URL inside a collection: `{collection}/{uid}.vcf`.
fn contact_url(collection_url: &str, uid: &str) -> String {
    format!("{}/{}.vcf", collection_url.trim_end_matches('/'), uid)
}

/// Build a vCard 3.0 from parsed fields.
pub fn build_vcard(
    uid: &str,
    full_name: &str,
    emails: &[String],
    phones: &[String],
    org: &str,
    title: &str,
) -> String {
    let mut v = String::from("BEGIN:VCARD\r\nVERSION:3.0\r\n");
    v.push_str(&format!("UID:{uid}\r\n"));
    if !full_name.is_empty() {
        v.push_str(&format!("FN:{}\r\n", vc_escape(full_name)));
    }
    v.push_str(&format!("N:;{};;;\r\n", vc_escape(full_name)));
    for e in emails.iter().filter(|s| !s.trim().is_empty()) {
        v.push_str(&format!("EMAIL:{}\r\n", vc_escape(e)));
    }
    for p in phones.iter().filter(|s| !s.trim().is_empty()) {
        v.push_str(&format!("TEL:{}\r\n", vc_escape(p)));
    }
    if !org.is_empty() {
        v.push_str(&format!("ORG:{}\r\n", vc_escape(org)));
    }
    if !title.is_empty() {
        v.push_str(&format!("TITLE:{}\r\n", vc_escape(title)));
    }
    v.push_str("END:VCARD\r\n");
    v
}

fn vc_escape(s: &str) -> String {
    s.replace('\\', "\\\\").replace(',', "\\,").replace(';', "\\;").replace('\n', "\\n")
}

/// PUT a vCard to create/replace a contact, honouring the precondition.
pub async fn put_contact(
    collection_url: &str,
    username: &str,
    password: &str,
    uid: &str,
    vcard: &str,
    pre: crate::caldav_client::Precondition<'_>,
) -> Result<(), String> {
    let http = http_client()?;
    let mut req = http
        .put(contact_url(collection_url, uid))
        .dav_auth(username, password)
        .header(reqwest::header::CONTENT_TYPE, "text/vcard; charset=utf-8");
    req = match pre {
        crate::caldav_client::Precondition::IfNew => req.header(reqwest::header::IF_NONE_MATCH, "*"),
        crate::caldav_client::Precondition::IfMatch(t) => req.header(reqwest::header::IF_MATCH, t),
        crate::caldav_client::Precondition::None => req,
    };
    let resp = req.body(vcard.to_string()).send().await.map_err(|e| format!("carddav PUT: {e}"))?;
    if resp.status().as_u16() == 412 {
        return Err("contact changed on the server — reopen and retry".into());
    }
    if !resp.status().is_success() {
        return Err(format!("carddav PUT HTTP {}", resp.status()));
    }
    Ok(())
}

/// GET one contact's raw .vcf + ETag.
pub async fn get_contact_raw(
    collection_url: &str,
    username: &str,
    password: &str,
    uid: &str,
) -> Result<(String, Option<String>), String> {
    let http = http_client()?;
    let resp = http
        .get(contact_url(collection_url, uid))
        .dav_auth(username, password)
        .send()
        .await
        .map_err(|e| format!("carddav GET: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!("carddav GET HTTP {}", resp.status()));
    }
    let etag = resp
        .headers()
        .get(reqwest::header::ETAG)
        .and_then(|v| v.to_str().ok())
        .map(String::from);
    let body = resp.text().await.map_err(|e| format!("carddav GET body: {e}"))?;
    Ok((body, etag))
}

/// DELETE a contact, optionally If-Match-guarded.
pub async fn delete_contact(
    collection_url: &str,
    username: &str,
    password: &str,
    uid: &str,
    if_match: Option<&str>,
) -> Result<(), String> {
    let http = http_client()?;
    let mut req = http
        .delete(contact_url(collection_url, uid))
        .dav_auth(username, password);
    if let Some(t) = if_match {
        req = req.header(reqwest::header::IF_MATCH, t);
    }
    let resp = req.send().await.map_err(|e| format!("carddav DELETE: {e}"))?;
    if !resp.status().is_success() && resp.status().as_u16() != 404 {
        return Err(format!("carddav DELETE HTTP {}", resp.status()));
    }
    Ok(())
}

fn http_client() -> Result<reqwest::Client, String> {
    reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(20))
        .build()
        .map_err(|e| format!("http client: {e}"))
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
    let mut uid = String::new();
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
            "UID" => uid = value.to_string(),
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
        account_key: String::new(),
        uid,
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

#[cfg(test)]
mod tests {
    use super::*;

    const MULTISTATUS: &str = r#"<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:response>
    <D:href>/carddav/1/addressbooks/2/a.vcf</D:href>
    <D:propstat>
      <D:prop>
        <D:getetag>"e1"</D:getetag>
        <C:address-data>BEGIN:VCARD
VERSION:3.0
FN:Ivan Petrov
EMAIL;TYPE=work:ivan@example.com
ORG:Acme;Sales
END:VCARD</C:address-data>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>"#;

    #[test]
    fn extracts_and_parses_one_card() {
        let cards = extract_address_data(MULTISTATUS);
        assert_eq!(cards.len(), 1, "one address-data block");
        let c = parse_vcard(&cards[0]).expect("parsed");
        assert_eq!(c.full_name, "Ivan Petrov");
        assert_eq!(c.emails, vec!["ivan@example.com"]);
        assert_eq!(c.organization, "Acme");
    }

    #[test]
    fn self_closing_address_data_is_skipped() {
        // The <C:address-data/> in a request/prop template must not be picked
        // up as a contact.
        let xml = r#"<D:prop><C:address-data/></D:prop>"#;
        assert!(extract_address_data(xml).is_empty());
    }

    #[test]
    fn vcard_line_folding_and_unescape() {
        let cards = extract_address_data(
            "<C:address-data>BEGIN:VCARD\nFN:Long\n  Name\nEMAIL:a&amp;b@x.ru\nEND:VCARD</C:address-data>",
        );
        let c = parse_vcard(&cards[0]).unwrap();
        assert_eq!(c.full_name, "LongName");
        assert_eq!(c.emails, vec!["a&b@x.ru"]);
    }

    #[test]
    fn card_without_identity_is_dropped() {
        assert!(parse_vcard("BEGIN:VCARD\nNOTE:hi\nEND:VCARD").is_none());
    }
}
