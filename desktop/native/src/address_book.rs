//! Address book (`view-mode 2`): fetching contacts, list rows and avatars,
//! the contact editor's payload.

use super::*;

/// Ask the engine for the address book. Empty query = full book, otherwise a
/// search. Answers are guarded UI-side by the echoed query (see the
/// EngineResult::Contacts handler), so typing fast just drops stale results.
pub(crate) fn fetch_contacts(sh: &Shared, query: &str) {
    if let Some(etx) = sh.engine_tx.borrow().as_ref() {
        let limit = if query.trim().is_empty() { 500 } else { 50 };
        let _ = etx.send(engine::EngineCmd::FetchContacts { query: query.to_string(), limit });
    }
}

/// Two-letter initials for the avatar bubble: first letters of the first two
/// whitespace-separated words, else the first char, uppercased.
pub(crate) fn contact_initials(name: &str) -> String {
    let words: Vec<&str> = name.split_whitespace().collect();
    let s: String = match words.as_slice() {
        [] => String::new(),
        [one] => one.chars().take(1).collect(),
        [a, b, ..] => a.chars().take(1).chain(b.chars().take(1)).collect(),
    };
    s.to_uppercase()
}

/// Stable pastel pick for a contact bubble, hashed off a seed (email or name).
pub(crate) fn contact_pastel(seed: &str) -> &'static str {
    let h = seed.bytes().fold(0u32, |acc, b| acc.wrapping_mul(31).wrapping_add(b as u32));
    IDENT_PASTEL[(h as usize) % IDENT_PASTEL.len()]
}

/// Build the Slint address-book rows from the engine's contact DTOs.
/// Build a contact-write body from the editor fields (one email/phone slot in
/// the v1 form; the server/model accept arrays).
pub(crate) fn contact_body_from_ui(ui: &MainWindow) -> serde_json::Value {
    let email = ui.get_ce_email().trim().to_string();
    let phone = ui.get_ce_phone().trim().to_string();
    let emails: Vec<String> = if email.is_empty() { vec![] } else { vec![email] };
    let phones: Vec<String> = if phone.is_empty() { vec![] } else { vec![phone] };
    serde_json::json!({
        "full_name": ui.get_ce_name().trim().to_string(),
        "emails": emails,
        "phones": phones,
        "organization": ui.get_ce_org().trim().to_string(),
    })
}

pub(crate) fn address_book_rows(list: &[ddmail_core::types::DesktopContact]) -> Vec<AddrBookRow> {
    list.iter()
        .map(|c| {
            let email = c.emails.first().cloned().unwrap_or_default();
            let has_name = !c.full_name.trim().is_empty();
            // With a real name, the second line is the email. Without one, the
            // email becomes the title and the second line falls back to the
            // organization (usually empty) — never repeat the email twice.
            let name = if has_name {
                c.full_name.clone()
            } else if !email.is_empty() {
                email.clone()
            } else {
                c.organization.clone()
            };
            let detail = if has_name { email.clone() } else { c.organization.clone() };
            let seed = if email.is_empty() { name.clone() } else { email.clone() };
            AddrBookRow {
                name: name.clone().into(),
                detail: detail.into(),
                initials: contact_initials(&name).into(),
                color: parse_hex_color(contact_pastel(&seed)).into(),
                email: email.into(),
            }
        })
        .collect()
}
