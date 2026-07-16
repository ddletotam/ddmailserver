//! Minimal read-only CalDAV client so the desktop can show a calendar from a
//! PLAIN server (not just our native backend). Phase 4, slice 2.
//!
//! Scope: a direct calendar-collection URL (as Yandex stores them) + a
//! calendar-query time-range REPORT. VEVENT fields are parsed as-is; RRULE is
//! passed through verbatim (the frontend expands recurrences, same as the
//! native path). No autodiscovery, no write, no timezone database — DTSTART
//! is resolved as UTC / all-day / floating-as-UTC. Basic auth.

use chrono::{NaiveDate, NaiveDateTime, TimeZone, Utc};

use crate::types::DesktopCalendarEvent;

/// Applies DAV auth to a request. Convention (keeps the (username, password)
/// signatures unchanged across the clients): an empty username with a password
/// of `"Bearer <token>"` sends an Authorization header (OAuth); anything else
/// is HTTP Basic. Callers building OAuth requests pass ("", "Bearer <token>").
pub(crate) trait DavAuthExt {
    fn dav_auth(self, username: &str, password: &str) -> Self;
}

impl DavAuthExt for reqwest::RequestBuilder {
    fn dav_auth(self, username: &str, password: &str) -> Self {
        if username.is_empty() && password.starts_with("Bearer ") {
            self.header(reqwest::header::AUTHORIZATION, password)
        } else {
            self.basic_auth(username, Some(password))
        }
    }
}

/// Fetch events overlapping `[from_ms, to_ms)` from a calendar-collection URL.
pub async fn fetch_events(
    calendar_url: &str,
    username: &str,
    password: &str,
    from_ms: i64,
    to_ms: i64,
) -> Result<Vec<DesktopCalendarEvent>, String> {
    let start = fmt_utc_stamp(from_ms);
    let end = fmt_utc_stamp(to_ms);
    let body = format!(
        r#"<?xml version="1.0" encoding="utf-8"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><D:getetag/><C:calendar-data/></D:prop>
  <C:filter>
    <C:comp-filter name="VCALENDAR">
      <C:comp-filter name="VEVENT">
        <C:time-range start="{start}" end="{end}"/>
      </C:comp-filter>
    </C:comp-filter>
  </C:filter>
</C:calendar-query>"#
    );

    let http = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(20))
        .build()
        .map_err(|e| format!("http client: {e}"))?;
    let method = reqwest::Method::from_bytes(b"REPORT").map_err(|e| format!("method: {e}"))?;
    let resp = http
        .request(method, calendar_url)
        .dav_auth(username, password)
        .header("Depth", "1")
        .header(reqwest::header::CONTENT_TYPE, "application/xml; charset=utf-8")
        .body(body)
        .send()
        .await
        .map_err(|e| format!("caldav REPORT: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!("caldav REPORT HTTP {}", resp.status()));
    }
    let xml = resp.text().await.map_err(|e| format!("caldav body: {e}"))?;

    let mut out = Vec::new();
    for cal in extract_tag(&xml, "calendar-data") {
        // One VCALENDAR may hold several VEVENTs (a master + its overrides).
        for vevent in split_vevents(&cal) {
            if let Some(ev) = parse_vevent(&vevent) {
                out.push(ev);
            }
        }
    }
    Ok(out)
}

/// Resolve a configured CalDAV URL to a concrete calendar-collection URL.
/// If the URL is already a calendar collection (or a server that returns
/// events directly) this is a cheap PROPFIND; otherwise it walks
/// current-user-principal → calendar-home-set → first calendar collection
/// (RFC 6764 discovery). Returns the input unchanged if discovery finds
/// nothing (so a direct collection URL, Yandex-style, still just works).
pub async fn resolve_calendar_collection(
    url: &str,
    username: &str,
    password: &str,
) -> Result<String, String> {
    // Is the URL itself a calendar collection?
    let root = propfind(url, username, password, 0, PROP_RESOURCETYPE_HOME).await?;
    if root.to_lowercase().contains("<c:calendar") || root.to_lowercase().contains(":calendar/>")
    {
        return Ok(url.to_string());
    }
    // Walk principal → calendar-home-set → first calendar.
    let principal = first_href(&root, "current-user-principal").unwrap_or_else(|| url.to_string());
    let principal = abs_url(url, &principal);
    let home_xml = propfind(&principal, username, password, 0, PROP_CAL_HOME).await?;
    let Some(home) = first_href(&home_xml, "calendar-home-set") else {
        return Ok(url.to_string());
    };
    let home = abs_url(url, &home);
    let listing = propfind(&home, username, password, 1, PROP_RESOURCETYPE_HOME).await?;
    // Pick the first response whose resourcetype is a calendar.
    for resp in split_responses(&listing) {
        if resp.to_lowercase().contains("calendar")
            && resp.to_lowercase().contains("resourcetype")
        {
            if let Some(href) = first_href(&resp, "href") {
                return Ok(abs_url(&home, &href));
            }
        }
    }
    Ok(url.to_string())
}

const PROP_RESOURCETYPE_HOME: &str = r#"<D:prop><D:resourcetype/><D:current-user-principal/><D:displayname/></D:prop>"#;
const PROP_CAL_HOME: &str = r#"<D:prop><C:calendar-home-set xmlns:C="urn:ietf:params:xml:ns:caldav"/></D:prop>"#;

/// One PROPFIND round-trip returning the raw multistatus XML.
pub(crate) async fn propfind(
    url: &str,
    username: &str,
    password: &str,
    depth: u8,
    prop: &str,
) -> Result<String, String> {
    let body = format!(
        r#"<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:">{prop}</D:propfind>"#
    );
    let http = client()?;
    let method = reqwest::Method::from_bytes(b"PROPFIND").map_err(|e| format!("method: {e}"))?;
    let resp = http
        .request(method, url)
        .dav_auth(username, password)
        .header("Depth", depth.to_string())
        .header(reqwest::header::CONTENT_TYPE, "application/xml; charset=utf-8")
        .body(body)
        .send()
        .await
        .map_err(|e| format!("PROPFIND: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!("PROPFIND HTTP {}", resp.status()));
    }
    resp.text().await.map_err(|e| format!("PROPFIND body: {e}"))
}

/// Split a multistatus into its `<response>…</response>` chunks.
pub(crate) fn split_responses(xml: &str) -> Vec<String> {
    extract_tag(xml, "response")
}

/// First `<…:TAG><…:href>VALUE</href>` inside `xml` (or a bare href for
/// tag="href"). Namespace-prefix agnostic.
pub(crate) fn first_href(xml: &str, tag: &str) -> Option<String> {
    if tag == "href" {
        return extract_tag(xml, "href").into_iter().next().map(|s| s.trim().to_string());
    }
    for block in extract_tag(xml, tag) {
        if let Some(h) = extract_tag(&block, "href").into_iter().next() {
            return Some(h.trim().to_string());
        }
    }
    None
}

/// Resolve a possibly-relative href against a base URL's origin.
pub(crate) fn abs_url(base: &str, href: &str) -> String {
    if href.starts_with("http://") || href.starts_with("https://") {
        return href.to_string();
    }
    // Scheme + host from base; href is an absolute path.
    let after_scheme = base.split("://").nth(1).unwrap_or(base);
    let host = after_scheme.split('/').next().unwrap_or("");
    let scheme = base.split("://").next().unwrap_or("https");
    if href.starts_with('/') {
        format!("{scheme}://{host}{href}")
    } else {
        format!("{}/{}", base.trim_end_matches('/'), href)
    }
}

/// Stable, positive, non-zero synthetic event id derived from the iCal UID
/// (CalDAV addresses events by UID; our UI routes by i64 id). FNV-1a >> 1.
pub fn event_id_from_uid(uid: &str) -> i64 {
    let mut h: u64 = 0xcbf29ce484222325;
    for b in uid.bytes() {
        h ^= b as u64;
        h = h.wrapping_mul(0x100000001b3);
    }
    (h >> 1) as i64
}

/// The event resource URL inside a collection: `{collection}/{uid}.ics`.
fn event_url(collection_url: &str, uid: &str) -> String {
    let base = collection_url.trim_end_matches('/');
    format!("{base}/{}.ics", uid)
}

/// Optimistic-concurrency precondition for a write.
pub enum Precondition<'a> {
    /// Create-only: fail if the resource already exists (If-None-Match: *).
    IfNew,
    /// Update-only: fail if it changed since we read this ETag (If-Match).
    IfMatch(&'a str),
    /// No precondition (last-write-wins).
    None,
}

/// PUT a full VCALENDAR to create/replace an event, honouring `pre`.
/// A 412 (precondition failed) maps to a clear "changed on the server" error.
pub async fn put_event(
    collection_url: &str,
    username: &str,
    password: &str,
    uid: &str,
    ical: &str,
    pre: Precondition<'_>,
) -> Result<(), String> {
    let http = client()?;
    let mut req = http
        .put(event_url(collection_url, uid))
        .dav_auth(username, password)
        .header(reqwest::header::CONTENT_TYPE, "text/calendar; charset=utf-8");
    req = match pre {
        Precondition::IfNew => req.header(reqwest::header::IF_NONE_MATCH, "*"),
        Precondition::IfMatch(tag) => req.header(reqwest::header::IF_MATCH, tag),
        Precondition::None => req,
    };
    let resp = req
        .body(ical.to_string())
        .send()
        .await
        .map_err(|e| format!("caldav PUT: {e}"))?;
    if resp.status().as_u16() == 412 {
        return Err("event changed on the server — reopen the calendar and retry".into());
    }
    if !resp.status().is_success() {
        return Err(format!("caldav PUT HTTP {}", resp.status()));
    }
    Ok(())
}

/// DELETE an event resource, optionally guarded by If-Match.
pub async fn delete_event(
    collection_url: &str,
    username: &str,
    password: &str,
    uid: &str,
    if_match: Option<&str>,
) -> Result<(), String> {
    let http = client()?;
    let mut req = http
        .delete(event_url(collection_url, uid))
        .dav_auth(username, password);
    if let Some(tag) = if_match {
        req = req.header(reqwest::header::IF_MATCH, tag);
    }
    let resp = req.send().await.map_err(|e| format!("caldav DELETE: {e}"))?;
    if resp.status().as_u16() == 412 {
        return Err("event changed on the server — reopen the calendar and retry".into());
    }
    // 404 is fine — already gone.
    if !resp.status().is_success() && resp.status().as_u16() != 404 {
        return Err(format!("caldav DELETE HTTP {}", resp.status()));
    }
    Ok(())
}

/// GET one event's raw .ics plus its current ETag (for fetch-merge-put and
/// If-Match-guarded delete).
pub async fn get_event_raw(
    collection_url: &str,
    username: &str,
    password: &str,
    uid: &str,
) -> Result<(String, Option<String>), String> {
    let http = client()?;
    let resp = http
        .get(event_url(collection_url, uid))
        .dav_auth(username, password)
        .send()
        .await
        .map_err(|e| format!("caldav GET: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!("caldav GET HTTP {}", resp.status()));
    }
    let etag = resp
        .headers()
        .get(reqwest::header::ETAG)
        .and_then(|v| v.to_str().ok())
        .map(|s| s.to_string());
    let body = resp.text().await.map_err(|e| format!("caldav GET body: {e}"))?;
    Ok((body, etag))
}

fn client() -> Result<reqwest::Client, String> {
    reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(20))
        .build()
        .map_err(|e| format!("http client: {e}"))
}

/// Build a minimal VCALENDAR/VEVENT for a brand-new event.
pub fn build_ical(
    uid: &str,
    summary: &str,
    description: &str,
    location: &str,
    dtstart_ms: i64,
    dtend_ms: Option<i64>,
    all_day: bool,
) -> String {
    let dtstamp = fmt_utc_stamp(dtstart_ms);
    let mut ev = String::from("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//ddmail//caldav//EN\r\nBEGIN:VEVENT\r\n");
    ev.push_str(&format!("UID:{}\r\n", uid));
    ev.push_str(&format!("DTSTAMP:{}\r\n", dtstamp));
    ev.push_str(&fmt_dt_line("DTSTART", dtstart_ms, all_day));
    if let Some(end) = dtend_ms {
        ev.push_str(&fmt_dt_line("DTEND", end, all_day));
    }
    if !summary.is_empty() {
        ev.push_str(&format!("SUMMARY:{}\r\n", escape_text(summary)));
    }
    if !description.is_empty() {
        ev.push_str(&format!("DESCRIPTION:{}\r\n", escape_text(description)));
    }
    if !location.is_empty() {
        ev.push_str(&format!("LOCATION:{}\r\n", escape_text(location)));
    }
    ev.push_str("END:VEVENT\r\nEND:VCALENDAR\r\n");
    ev
}

/// Apply changed fields to an existing .ics, preserving everything else
/// (UID, RRULE, attendees…). Used by patch. Replaces the matching property
/// lines and inserts any that were absent, just before END:VEVENT.
pub fn merge_ical(
    existing: &str,
    summary: &str,
    description: &str,
    location: &str,
    dtstart_ms: i64,
    dtend_ms: Option<i64>,
    all_day: bool,
) -> String {
    let mut updates: Vec<(String, String)> = vec![
        ("SUMMARY".into(), format!("SUMMARY:{}", escape_text(summary))),
        ("DESCRIPTION".into(), format!("DESCRIPTION:{}", escape_text(description))),
        ("LOCATION".into(), format!("LOCATION:{}", escape_text(location))),
        ("DTSTART".into(), fmt_dt_line("DTSTART", dtstart_ms, all_day).trim_end().to_string()),
    ];
    match dtend_ms {
        Some(end) => updates.push(("DTEND".into(), fmt_dt_line("DTEND", end, all_day).trim_end().to_string())),
        None => updates.push(("DTEND".into(), String::new())), // drop DTEND
    }

    let mut seen: std::collections::HashSet<String> = std::collections::HashSet::new();
    let mut out = String::new();
    for line in existing.replace("\r\n", "\n").lines() {
        let prop = line.split([';', ':']).next().unwrap_or("").to_ascii_uppercase();
        if let Some((_, repl)) = updates.iter().find(|(k, _)| *k == prop) {
            seen.insert(prop.clone());
            if !repl.is_empty() {
                out.push_str(repl);
                out.push_str("\r\n");
            }
            continue; // replaced or dropped
        }
        out.push_str(line);
        out.push_str("\r\n");
    }
    // Insert any updated props that weren't already present, before END:VEVENT.
    let missing: String = updates
        .iter()
        .filter(|(k, v)| !seen.contains(k) && !v.is_empty())
        .map(|(_, v)| format!("{v}\r\n"))
        .collect();
    if !missing.is_empty() {
        out = out.replacen("END:VEVENT", &format!("{missing}END:VEVENT"), 1);
    }
    out
}

fn fmt_dt_line(prop: &str, ms: i64, all_day: bool) -> String {
    if all_day {
        let d = Utc.timestamp_millis_opt(ms).single().unwrap_or_else(Utc::now);
        format!("{prop};VALUE=DATE:{}\r\n", d.format("%Y%m%d"))
    } else {
        format!("{prop}:{}\r\n", fmt_utc_stamp(ms))
    }
}

fn escape_text(s: &str) -> String {
    s.replace('\\', "\\\\")
        .replace('\n', "\\n")
        .replace(',', "\\,")
        .replace(';', "\\;")
}

/// `20260715T120000Z` — the UTC stamp CalDAV time-range filters want.
fn fmt_utc_stamp(ms: i64) -> String {
    Utc.timestamp_millis_opt(ms)
        .single()
        .unwrap_or_else(Utc::now)
        .format("%Y%m%dT%H%M%SZ")
        .to_string()
}

/// Pull each `<…:TAG>…</…:TAG>` payload out of XML, prefix-agnostic, unescaped.
/// Skips self-closing tags. (Shared shape with the CardDAV client's extractor.)
fn extract_tag(xml: &str, tag: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut rest = xml;
    while let Some(pos) = rest.find(tag) {
        let before = &rest[..pos];
        let is_close = before.trim_end().ends_with("</") || before.trim_end().ends_with('/');
        let after = &rest[pos + tag.len()..];
        let Some(gt) = after.find('>') else { break };
        if is_close || after[..gt].trim_end().ends_with('/') {
            rest = &after[gt + 1..];
            continue;
        }
        let content_start = pos + tag.len() + gt + 1;
        let region = &rest[content_start..];
        let Some(close_rel) = region.find(tag) else { break };
        let raw = &region[..close_rel];
        let content = raw.rsplit_once('<').map(|(a, _)| a).unwrap_or(raw);
        let trimmed = content.trim();
        if !trimmed.is_empty() {
            out.push(xml_unescape(trimmed));
        }
        rest = &region[close_rel + tag.len()..];
    }
    out
}

/// Split a VCALENDAR body into its VEVENT blocks (BEGIN:VEVENT … END:VEVENT).
fn split_vevents(vcal: &str) -> Vec<String> {
    let unfolded = unfold(vcal);
    let mut out = Vec::new();
    let mut cur: Option<String> = None;
    for line in unfolded.lines() {
        let l = line.trim_end();
        if l == "BEGIN:VEVENT" {
            cur = Some(String::new());
        } else if l == "END:VEVENT" {
            if let Some(b) = cur.take() {
                out.push(b);
            }
        } else if let Some(b) = cur.as_mut() {
            b.push_str(l);
            b.push('\n');
        }
    }
    out
}

fn parse_vevent(vevent: &str) -> Option<DesktopCalendarEvent> {
    let mut uid = String::new();
    let mut summary = String::new();
    let mut description = String::new();
    let mut location = String::new();
    let mut status = String::new();
    let mut rrule = String::new();
    let mut recurrence_id = String::new();
    let mut dtstart: Option<i64> = None;
    let mut dtend: Option<i64> = None;
    let mut all_day = false;

    for line in vevent.lines() {
        let Some((head, value)) = line.split_once(':') else { continue };
        let value = value.trim();
        let name = head.split(';').next().unwrap_or("").to_ascii_uppercase();
        match name.as_str() {
            "UID" => uid = value.to_string(),
            "SUMMARY" => summary = unescape_text(value),
            "DESCRIPTION" => description = unescape_text(value),
            "LOCATION" => location = unescape_text(value),
            "STATUS" => status = value.to_string(),
            "RRULE" => rrule = value.to_string(),
            "RECURRENCE-ID" => recurrence_id = value.to_string(),
            "DTSTART" => {
                if let Some((ms, ad)) = parse_ical_dt(head, value) {
                    dtstart = Some(ms);
                    all_day = ad;
                }
            }
            "DTEND" => {
                if let Some((ms, _)) = parse_ical_dt(head, value) {
                    dtend = Some(ms);
                }
            }
            _ => {}
        }
    }

    let dtstart = dtstart?;
    Some(DesktopCalendarEvent {
        id: 0,
        calendar_id: 0, // set by the caller (one synthetic calendar per URL)
        uid,
        summary,
        description,
        location,
        dtstart,
        dtend,
        all_day,
        organizer_email: String::new(),
        organizer_name: String::new(),
        status,
        rrule,
        recurrence_id,
        exdates: Vec::new(),
        attendees: Vec::new(),
        alarm_lead_min: 0,
        alarm_leads: Vec::new(),
        extras: Vec::new(),
        color: String::new(),
        editable: false,
        deletable: false,
        identity_email: String::new(),
    })
}

/// Parse an iCal DTSTART/DTEND value to (ms-since-epoch, all_day).
/// Handles `VALUE=DATE` (all-day), trailing-Z UTC, and floating time (treated
/// as UTC — no VTIMEZONE resolution in this slice).
fn parse_ical_dt(head: &str, value: &str) -> Option<(i64, bool)> {
    let is_date = head.to_ascii_uppercase().contains("VALUE=DATE") || value.len() == 8;
    if is_date {
        let d = NaiveDate::parse_from_str(value, "%Y%m%d").ok()?;
        let dt = d.and_hms_opt(0, 0, 0)?;
        return Some((Utc.from_utc_datetime(&dt).timestamp_millis(), true));
    }
    let v = value.trim_end_matches('Z');
    let dt = NaiveDateTime::parse_from_str(v, "%Y%m%dT%H%M%S").ok()?;
    Some((Utc.from_utc_datetime(&dt).timestamp_millis(), false))
}

/// iCal line unfolding (RFC 5545 §3.1): a leading space/tab continues the
/// previous line.
fn unfold(s: &str) -> String {
    let mut out = String::new();
    for line in s.replace("\r\n", "\n").lines() {
        if line.starts_with(' ') || line.starts_with('\t') {
            out.push_str(&line[1..]);
        } else {
            if !out.is_empty() {
                out.push('\n');
            }
            out.push_str(line);
        }
    }
    out
}

/// iCal TEXT unescaping (RFC 5545 §3.3.11).
fn unescape_text(s: &str) -> String {
    s.replace("\\n", "\n")
        .replace("\\N", "\n")
        .replace("\\,", ",")
        .replace("\\;", ";")
        .replace("\\\\", "\\")
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

    #[test]
    fn parses_utc_vevent() {
        let ve = "UID:abc-1\nSUMMARY:Standup\nDTSTART:20260715T090000Z\nDTEND:20260715T093000Z\nRRULE:FREQ=WEEKLY";
        let e = parse_vevent(ve).unwrap();
        assert_eq!(e.uid, "abc-1");
        assert_eq!(e.summary, "Standup");
        assert!(!e.all_day);
        assert_eq!(e.rrule, "FREQ=WEEKLY");
        assert!(e.dtend.unwrap() > e.dtstart);
    }

    #[test]
    fn parses_all_day() {
        let ve = "UID:d1\nSUMMARY:Holiday\nDTSTART;VALUE=DATE:20260101";
        let e = parse_vevent(ve).unwrap();
        assert!(e.all_day);
        assert_eq!(e.summary, "Holiday");
    }

    #[test]
    fn unescapes_text_and_unfolds() {
        let vcal = "BEGIN:VEVENT\nUID:x\nSUMMARY:Line one\\, part\nDESCRIPTION:aaa\n bbb\nDTSTART:20260715T120000Z\nEND:VEVENT";
        let evs = split_vevents(vcal);
        assert_eq!(evs.len(), 1);
        let e = parse_vevent(&evs[0]).unwrap();
        assert_eq!(e.summary, "Line one, part");
        assert_eq!(e.description, "aaabbb");
    }

    #[test]
    fn builds_ical_with_escaped_fields() {
        let ics = build_ical("u1", "Plan, v2", "", "Room; A", 1_700_000_000_000, Some(1_700_003_600_000), false);
        assert!(ics.contains("UID:u1"));
        assert!(ics.contains("SUMMARY:Plan\\, v2"));
        assert!(ics.contains("LOCATION:Room\\; A"));
        assert!(ics.contains("DTSTART:"));
        assert!(ics.contains("DTEND:"));
        // Round-trips back through our own parser.
        let e = parse_vevent(&split_vevents(&ics)[0]).unwrap();
        assert_eq!(e.summary, "Plan, v2");
        assert_eq!(e.location, "Room; A");
    }

    #[test]
    fn merge_preserves_rrule_and_uid_replaces_summary() {
        let existing = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:keep-me\r\nSUMMARY:Old\r\nRRULE:FREQ=WEEKLY\r\nDTSTART:20260715T090000Z\r\nDTEND:20260715T093000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n";
        let merged = merge_ical(existing, "New title", "", "", 1_700_000_000_000, None, false);
        assert!(merged.contains("UID:keep-me"), "uid preserved");
        assert!(merged.contains("RRULE:FREQ=WEEKLY"), "rrule preserved");
        assert!(merged.contains("SUMMARY:New title"), "summary replaced");
        assert!(!merged.contains("SUMMARY:Old"));
        assert!(!merged.contains("DTEND:"), "dtend dropped when None");
    }

    #[test]
    fn discovery_href_and_abs_url() {
        let xml = r#"<D:multistatus xmlns:D="DAV:"><D:response><D:href>/p/</D:href>
          <D:propstat><D:prop><D:current-user-principal><D:href>/principals/lucky/</D:href></D:current-user-principal></D:prop></D:propstat>
        </D:response></D:multistatus>"#;
        assert_eq!(first_href(xml, "current-user-principal").as_deref(), Some("/principals/lucky/"));
        assert_eq!(
            abs_url("https://mail.letotam.ru/caldav/", "/principals/lucky/"),
            "https://mail.letotam.ru/principals/lucky/"
        );
        assert_eq!(
            abs_url("https://x.ru/a/", "https://other.ru/b/"),
            "https://other.ru/b/"
        );
    }

    #[test]
    fn event_id_is_stable_positive_nonzero() {
        let a = event_id_from_uid("abc@x");
        assert_eq!(a, event_id_from_uid("abc@x"));
        assert!(a > 0);
        assert_ne!(a, event_id_from_uid("different"));
    }

    #[test]
    fn extracts_calendar_data_and_splits_multiple_vevents() {
        let xml = "<C:calendar-data>BEGIN:VCALENDAR\nBEGIN:VEVENT\nUID:a\nDTSTART:20260715T120000Z\nEND:VEVENT\nBEGIN:VEVENT\nUID:b\nDTSTART:20260716T120000Z\nEND:VEVENT\nEND:VCALENDAR</C:calendar-data>";
        let cals = extract_tag(xml, "calendar-data");
        assert_eq!(cals.len(), 1);
        assert_eq!(split_vevents(&cals[0]).len(), 2);
    }
}
