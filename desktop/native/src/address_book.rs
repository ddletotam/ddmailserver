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

/// Address-book callbacks: search, open a conversation with a contact,
/// the contact editor (add / edit / save / delete).
pub(crate) fn wire_address_book(ui: &MainWindow, shared: &Rc<Shared>) {
    // Address-book search box: fire the lookup on every edit (engine answers
    // are guarded by the echoed query, so stale results are dropped).
    let sh_cs = shared.clone();
    ui.on_contacts_search(move |q| {
        fetch_contacts(&sh_cs, q.as_str());
    });

    // Click a contact row → jump to a compose addressed to them.
    let ui_weak_ca = ui.as_weak();
    ui.on_contact_activated(move |email| {
        let Some(ui) = ui_weak_ca.upgrade() else { return };
        if email.is_empty() {
            return;
        }
        ui.set_view_mode(0);
        ui.invoke_search_compose_new(email);
    });

    // Contact editor: open blank (create).
    let ui_weak_cadd = ui.as_weak();
    let sh_cadd = shared.clone();
    ui.on_contact_add(move || {
        let Some(ui) = ui_weak_cadd.upgrade() else { return };
        sh_cadd.editing_contact_id.set(0);
        sh_cadd.editing_contact_account.borrow_mut().clear();
        // Populate the account picker (labels + parallel keys).
        {
            let accounts = engine::AccountConfig::load_all();
            let labels: Vec<slint::SharedString> = accounts
                .iter()
                .map(|a| if a.email.is_empty() { a.account_key() } else { a.email.clone() }.into())
                .collect();
            *sh_cadd.ce_account_keys.borrow_mut() =
                accounts.iter().map(|a| a.account_key()).collect();
            ui.set_ce_accounts(ModelRc::new(VecModel::from(labels)));
            ui.set_ce_account_idx(0);
        }
        ui.set_ce_is_edit(false);
        ui.set_ce_name("".into());
        ui.set_ce_email("".into());
        ui.set_ce_phone("".into());
        ui.set_ce_org("".into());
        ui.set_contact_editor_open(true);
    });

    // Contact editor: open populated for a row (edit).
    let ui_weak_ced = ui.as_weak();
    let sh_ced = shared.clone();
    ui.on_contact_edit(move |idx| {
        let Some(ui) = ui_weak_ced.upgrade() else { return };
        let book = sh_ced.address_book.borrow();
        let Some(c) = book.get(idx.max(0) as usize) else { return };
        sh_ced.editing_contact_id.set(c.id);
        *sh_ced.editing_contact_account.borrow_mut() = c.account_key.clone();
        ui.set_ce_is_edit(true);
        ui.set_ce_name(c.full_name.clone().into());
        ui.set_ce_email(c.emails.first().cloned().unwrap_or_default().into());
        ui.set_ce_phone(c.phones.first().cloned().unwrap_or_default().into());
        ui.set_ce_org(c.organization.clone().into());
        ui.set_contact_editor_open(true);
    });

    let ui_weak_ccancel = ui.as_weak();
    ui.on_contact_editor_cancel(move || {
        if let Some(ui) = ui_weak_ccancel.upgrade() {
            ui.set_contact_editor_open(false);
        }
    });

    // Save: create or update, then close and refresh the book.
    let ui_weak_csave = ui.as_weak();
    let sh_csave = shared.clone();
    ui.on_contact_save(move || {
        let Some(ui) = ui_weak_csave.upgrade() else { return };
        let body = contact_body_from_ui(&ui);
        let Some(etx) = sh_csave.engine_tx.borrow().clone() else { return };
        let id = sh_csave.editing_contact_id.get();
        let ak = if id == 0 {
            // Create → the account chosen in the picker.
            let idx = ui.get_ce_account_idx().max(0) as usize;
            sh_csave.ce_account_keys.borrow().get(idx).cloned().unwrap_or_default()
        } else {
            sh_csave.editing_contact_account.borrow().clone()
        };
        if id == 0 {
            let _ = etx.send(engine::EngineCmd::CreateContact { body, account_key: ak });
        } else {
            let _ = etx.send(engine::EngineCmd::UpdateContact { id, body, account_key: ak });
        }
        ui.set_contact_editor_open(false);
    });

    // Delete the contact being edited.
    let ui_weak_cdel = ui.as_weak();
    let sh_cdel = shared.clone();
    ui.on_contact_delete(move || {
        let Some(ui) = ui_weak_cdel.upgrade() else { return };
        let id = sh_cdel.editing_contact_id.get();
        if id != 0 {
            let ak = sh_cdel.editing_contact_account.borrow().clone();
            if let Some(etx) = sh_cdel.engine_tx.borrow().as_ref() {
                let _ = etx.send(engine::EngineCmd::DeleteContact { id, account_key: ak });
            }
        }
        ui.set_contact_editor_open(false);
    });
}
