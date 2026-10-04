//! The search dropdown above the conversation list (contract §4, «Поиск»):
//! local contact and conversation matching, row models for the three
//! sections, message hits from the server.

use super::*;

/// "Looks like an email" check for the live-dropdown compose-new row.
/// Matches the spec from svelte's SearchDropdown (EMAIL_RE): something
/// before @, something between @ and the last dot, something after.
/// Pulled into Rust so the Slint side stays declarative.
pub(crate) fn parse_email_like(q: &str) -> Option<String> {
    let s = q.trim();
    if s.contains(|c: char| c.is_whitespace() || c == '<' || c == '>' || c == '"' || c == ',') {
        return None;
    }
    let at = s.find('@')?;
    let local = &s[..at];
    let domain = &s[at + 1..];
    if local.is_empty() || domain.is_empty() {
        return None;
    }
    let dot = domain.find('.')?;
    if dot == 0 || dot == domain.len() - 1 {
        return None;
    }
    Some(s.to_lowercase())
}

/// Short "HH:MM" / "DD.MM" / "DD.MM.YY" formatter for the dropdown
/// message rows — matches svelte's formatDateShort behaviour closely
/// enough for the right-aligned date hint.
pub(crate) fn fmt_short_date(ts_ms: i64) -> String {
    if ts_ms <= 0 {
        return String::new();
    }
    use chrono::{DateTime, Datelike, Local, TimeZone, Timelike};
    let dt: DateTime<Local> = match Local.timestamp_millis_opt(ts_ms).single() {
        Some(d) => d,
        None => return String::new(),
    };
    let now = Local::now();
    if dt.year() == now.year() && dt.ordinal() == now.ordinal() {
        return format!("{:02}:{:02}", dt.hour(), dt.minute());
    }
    if dt.year() == now.year() {
        return format!("{:02}.{:02}", dt.day(), dt.month());
    }
    format!("{:02}.{:02}.{:02}", dt.day(), dt.month(), dt.year() % 100)
}

pub(crate) fn contact_items(contacts: &[Contact]) -> Vec<ContactItem> {
    contacts
        .iter()
        .map(|c| ContactItem { name: c.name.clone().into(), email: c.email.clone().into() })
        .collect()
}

/// Match `q_lc` (already lowercased) against the address book. Собеседники
/// диалогов сюда больше не идут — их находит секция «Диалоги»
/// (`local_search_convs`), и адрес, уже показанный там, из контактов
/// выбрасывается (`skip`). Case-insensitive and Unicode-aware (Rust
/// `to_lowercase`, unlike SQLite's ASCII-only `LOWER`, which never folded
/// Cyrillic). Returned as `Contact` so the dropdown row + select-flow reuse
/// unchanged; deduped by address, capped. Runs entirely client-side, so it
/// answers on the first keystroke without waiting for the network message
/// search.
pub(crate) fn local_search_contacts(
    book: &[ddmail_core::types::DesktopContact],
    skip: &HashSet<String>,
    q_lc: &str,
) -> Vec<Contact> {
    const CAP: usize = 12;
    let mut seen: HashSet<String> = skip.clone();
    let mut out: Vec<Contact> = Vec::new();

    for c in book {
        let hit = c.full_name.to_lowercase().contains(q_lc)
            || c.organization.to_lowercase().contains(q_lc)
            || c.emails.iter().any(|e| e.to_lowercase().contains(q_lc));
        if !hit {
            continue;
        }
        let email = c.emails.first().cloned().unwrap_or_default();
        let key = if email.is_empty() { c.full_name.to_lowercase() } else { email.to_lowercase() };
        if key.is_empty() || !seen.insert(key) {
            continue;
        }
        out.push(Contact {
            email,
            name: if c.full_name.trim().is_empty() {
                c.organization.clone()
            } else {
                c.full_name.clone()
            },
            source: "carddav".into(),
        });
        if out.len() >= CAP {
            return out;
        }
    }

    out
}

/// Строка секции «Диалоги» в выпадашке поиска. Диалог ищется по ключу, а
/// не по индексу: список может перестроиться, пока выпадашка открыта.
#[derive(Clone)]
pub(crate) struct ConvHit {
    pub(crate) account: String,
    pub(crate) id: String,
    pub(crate) name: String,
    /// Что совпало: тема последнего письма (совпало имя), адрес или тема.
    pub(crate) detail: String,
    /// Адреса собеседников — чтобы не повторять их в «Контактах».
    pub(crate) addrs: Vec<String>,
}

/// Локальный поиск диалогов (контракт §4, «Поиск»): по имени диалога (в том
/// числе пользовательскому), по адресам участников и по темам писем —
/// последней из списка и всех закэшированных тел (`subjects`, строки
/// `Cache::body_subjects`). Порядок — имя, адрес, тема; внутри — как в
/// сайдбаре, от свежего к старому. Сервер не спрашивается: его поиск писем
/// идёт своей секцией, как и раньше.
pub(crate) fn local_search_convs(
    convs: &[Conversation],
    subjects: &[(String, String, u32, String)],
    fallback: &str,
    q_lc: &str,
) -> Vec<ConvHit> {
    const CAP: usize = 8;
    let hit = |c: &Conversation, detail: String| ConvHit {
        account: eff_account(fallback, c),
        id: c.id.clone(),
        name: conv_name(c),
        detail,
        addrs: c.counterparts.iter().map(|cp| cp.addr.to_lowercase()).collect(),
    };
    let mut out: Vec<ConvHit> = Vec::new();
    let mut taken = vec![false; convs.len()];

    // 1) Имя диалога.
    for (i, c) in convs.iter().enumerate() {
        if conv_name(c).to_lowercase().contains(q_lc) {
            taken[i] = true;
            out.push(hit(c, c.last_subject.clone()));
        }
    }
    // 2) Адрес любого участника.
    for (i, c) in convs.iter().enumerate() {
        if taken[i] {
            continue;
        }
        if let Some(cp) = c.counterparts.iter().find(|cp| cp.addr.to_lowercase().contains(q_lc)) {
            taken[i] = true;
            out.push(hit(c, cp.addr.clone()));
        }
    }
    // 3) Тема письма: сначала последняя (она есть и без тел), затем кэш тел.
    let mut by_subject: Vec<(usize, String)> = Vec::new();
    for (i, c) in convs.iter().enumerate() {
        if !taken[i] && c.last_subject.to_lowercase().contains(q_lc) {
            taken[i] = true;
            by_subject.push((i, c.last_subject.clone()));
        }
    }
    let mut owner: HashMap<(&str, &str, u32), usize> = HashMap::new();
    let accounts: Vec<String> = convs.iter().map(|c| eff_account(fallback, c)).collect();
    for (i, c) in convs.iter().enumerate() {
        if taken[i] {
            continue;
        }
        for m in &c.messages {
            owner.insert((accounts[i].as_str(), m.folder.as_str(), m.uid), i);
        }
    }
    if !owner.is_empty() {
        for (acc, folder, uid, subject) in subjects {
            let Some(&i) = owner.get(&(acc.as_str(), folder.as_str(), *uid)) else { continue };
            if !taken[i] && subject.to_lowercase().contains(q_lc) {
                taken[i] = true;
                by_subject.push((i, subject.clone()));
            }
        }
    }
    by_subject.sort_by_key(|(i, _)| *i);
    for (i, subject) in by_subject {
        out.push(hit(&convs[i], format!("Тема: {subject}")));
    }
    out.truncate(CAP);
    out
}

/// Адреса собеседников найденных диалогов — «Контакты» их не повторяют.
pub(crate) fn conv_hit_addrs(hits: &[ConvHit]) -> HashSet<String> {
    hits.iter().flat_map(|h| h.addrs.iter().cloned()).collect()
}

pub(crate) fn conv_hit_items(hits: &[ConvHit]) -> Vec<ConvHitItem> {
    hits.iter()
        .map(|h| ConvHitItem { name: h.name.clone().into(), detail: h.detail.clone().into() })
        .collect()
}

/// Pull a short single-line preview out of a message body — first the
/// plain-text part, then collapsing HTML tags out of html when text is
/// missing. Whitespace squashed, capped at ~80 chars for the ribbon.
pub(crate) fn body_preview(body: &MessageBody) -> String {
    let raw = body
        .text
        .as_deref()
        .filter(|s| !s.trim().is_empty())
        .map(|s| s.to_string())
        .or_else(|| {
            body.html.as_deref().map(|h| {
                // Cheap tag strip — the ribbon needs at most a sentence.
                let mut out = String::with_capacity(h.len());
                let mut in_tag = false;
                for ch in h.chars() {
                    match ch {
                        '<' => in_tag = true,
                        '>' => in_tag = false,
                        _ if !in_tag => out.push(ch),
                        _ => {}
                    }
                }
                out
            })
        })
        .unwrap_or_default();
    let collapsed = raw.split_whitespace().collect::<Vec<_>>().join(" ");
    if collapsed.chars().count() <= 80 {
        collapsed
    } else {
        let cut: String = collapsed.chars().take(80).collect();
        format!("{cut}…")
    }
}

pub(crate) fn message_hits(envs: &[MessageEnvelope]) -> Vec<MessageHit> {
    envs.iter()
        .map(|e| MessageHit {
            from: if e.from.is_empty() { e.from_addr.clone() } else { e.from.clone() }.into(),
            subject: e.subject.clone().into(),
            date: fmt_short_date(e.date_ts).into(),
        })
        .collect()
}

#[cfg(test)]
mod conv_search_tests {
    use super::{conv_hit_addrs, conv_meta_parts, local_search_convs, recipients_tip};
    use ddmail_core::types::{ContactInfo, Conversation, MessageBody, MessageRef};

    fn conv(
        id: &str,
        label: &str,
        addrs: &[&str],
        last_subject: &str,
        refs: &[u32],
    ) -> Conversation {
        Conversation {
            id: id.into(),
            label: label.into(),
            avatar_hash: String::new(),
            received_by: "me@example.ru".into(),
            counterparts: addrs
                .iter()
                .map(|a| ContactInfo { name: String::new(), addr: (*a).into() })
                .collect(),
            is_group: addrs.len() > 1,
            last_date: String::new(),
            last_date_ts: 0,
            last_subject: last_subject.into(),
            unread_count: 0,
            total_count: refs.len() as u32,
            messages: refs
                .iter()
                .map(|&uid| MessageRef {
                    folder: "INBOX".into(),
                    uid,
                    message_id: String::new(),
                    seen: true,
                })
                .collect(),
            draft: None,
            account_key: String::new(),
            merged: false,
        }
    }

    fn sample() -> Vec<Conversation> {
        vec![
            conv("c0", "Иван Петров", &["ivan@example.ru"], "Счёт за сентябрь", &[1]),
            conv(
                "c1",
                "",
                &["anna@lizing.example.ru", "olga@lizing.example.ru"],
                "RE: выкуп",
                &[2, 3],
            ),
            conv("c2", "Бухгалтерия", &["buh@example.ru"], "Акт сверки", &[4]),
        ]
    }

    fn ids(q: &str, subjects: &[(String, String, u32, String)]) -> Vec<String> {
        local_search_convs(&sample(), subjects, "acc", q).into_iter().map(|h| h.id).collect()
    }

    #[test]
    fn finds_by_name_case_insensitive_cyrillic() {
        assert_eq!(ids("петров", &[]), vec!["c0"]);
    }

    #[test]
    fn finds_by_any_participant_address() {
        // Второй участник группы — не первый, по которому названа строка.
        let hits = local_search_convs(&sample(), &[], "acc", "olga@");
        assert_eq!(hits.len(), 1);
        assert_eq!(hits[0].id, "c1");
        assert_eq!(hits[0].detail, "olga@lizing.example.ru");
    }

    #[test]
    fn finds_by_cached_subject_not_only_the_last_one() {
        let subjects = vec![("acc".into(), "INBOX".into(), 2u32, "Договор №5 — график".into())];
        let hits = local_search_convs(&sample(), &subjects, "acc", "график");
        assert_eq!(hits.len(), 1);
        assert_eq!(hits[0].id, "c1");
        assert_eq!(hits[0].detail, "Тема: Договор №5 — график");
        // Тема чужого аккаунта с тем же (folder, uid) — не этот диалог.
        let foreign = vec![("other".into(), "INBOX".into(), 2u32, "график".into())];
        assert!(ids("график", &foreign).is_empty());
    }

    #[test]
    fn name_beats_address_beats_subject_and_no_duplicates() {
        // «example» есть в адресах всех трёх и в имени ни одного; «бух» —
        // и в имени c2, и в его адресе: диалог один раз, как совпадение имени.
        assert_eq!(ids("бух", &[]), vec!["c2"]);
        let subjects = vec![("acc".into(), "INBOX".into(), 1u32, "сверка".into())];
        // «свер»: c2 — последняя тема, c0 — тема из кэша; порядок как в сайдбаре.
        assert_eq!(ids("свер", &subjects), vec!["c0", "c2"]);
    }

    #[test]
    fn hit_addresses_are_excluded_from_contacts() {
        let hits = local_search_convs(&sample(), &[], "acc", "выкуп");
        let addrs = conv_hit_addrs(&hits);
        assert!(
            addrs.contains("anna@lizing.example.ru") && addrs.contains("olga@lizing.example.ru")
        );
    }

    #[test]
    fn header_meta_parts_are_clickable_addresses() {
        let c = conv("c1", "", &["anna@lizing.example.ru", "olga@lizing.example.ru"], "", &[]);
        let parts = conv_meta_parts(&c);
        let clickable: Vec<&str> =
            parts.iter().filter(|p| !p.addr.is_empty()).map(|p| p.addr.as_str()).collect();
        assert_eq!(
            clickable,
            vec!["anna@lizing.example.ru", "olga@lizing.example.ru", "me@example.ru"]
        );
        // Разделители не кликабельны и видны как текст.
        let text: String = parts.iter().map(|p| p.text.as_str()).collect();
        assert_eq!(text, "anna@lizing.example.ru, olga@lizing.example.ru → me@example.ru");
    }

    #[test]
    fn recipients_tip_lists_to_and_cc() {
        let mut b = MessageBody {
            uid: 1,
            folder: "Sent".into(),
            subject: String::new(),
            from: String::new(),
            from_addr: "me@example.ru".into(),
            to: vec!["anna@lizing.example.ru".into(), " olga@lizing.example.ru ".into()],
            cc: vec![],
            date: String::new(),
            date_ts: 0,
            html: None,
            text: None,
            attachments: vec![],
            is_outgoing: true,
            message_id: String::new(),
            in_reply_to: String::new(),
            references: vec![],
            raw_headers: String::new(),
        };
        assert_eq!(recipients_tip(&b), "Кому: anna@lizing.example.ru, olga@lizing.example.ru");
        b.cc = vec!["Бух <buh@example.ru>".into()];
        assert_eq!(
            recipients_tip(&b),
            "Кому: anna@lizing.example.ru, olga@lizing.example.ru\nКопия: Бух <buh@example.ru>"
        );
    }
}
