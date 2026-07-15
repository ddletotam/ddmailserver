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
        .basic_auth(username, Some(password))
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
    fn extracts_calendar_data_and_splits_multiple_vevents() {
        let xml = "<C:calendar-data>BEGIN:VCALENDAR\nBEGIN:VEVENT\nUID:a\nDTSTART:20260715T120000Z\nEND:VEVENT\nBEGIN:VEVENT\nUID:b\nDTSTART:20260716T120000Z\nEND:VEVENT\nEND:VCALENDAR</C:calendar-data>";
        let cals = extract_tag(xml, "calendar-data");
        assert_eq!(cals.len(), 1);
        assert_eq!(split_vevents(&cals[0]).len(), 2);
    }
}
