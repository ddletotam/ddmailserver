//! Серверы IMAP/SMTP по адресу почты — для формы подключения.
//!
//! Таблица короткая намеренно: только службы, которыми реально пользуются
//! и у которых серверы не выводятся из домена (`imap.mail.ru` обслуживает и
//! `bk.ru`, и `inbox.ru`). Для остальных — догадка `imap.<домен>` /
//! `smtp.<домен>`, которую форма помечает «проверьте».
//!
//! Outlook.com/Hotmail в таблице нет: Microsoft не пускает IMAP по паролю
//! (только OAuth2), и подставленные серверы обещали бы то, что не сработает.

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct Servers {
    pub imap_host: String,
    pub imap_port: u16,
    pub smtp_host: String,
    pub smtp_port: u16,
    /// Название службы, если она из таблицы; `None` — серверы угаданы.
    pub provider: Option<&'static str>,
    /// Что сказать про пароль: многие службы пускают внешние программы
    /// только по паролю приложения, а обычный отвергают как неверный.
    pub password_hint: &'static str,
}

struct Preset {
    domains: &'static [&'static str],
    name: &'static str,
    imap: &'static str,
    smtp: &'static str,
    hint: &'static str,
}

const PRESETS: &[Preset] = &[
    Preset {
        domains: &["mail.ru", "inbox.ru", "list.ru", "bk.ru", "internet.ru", "xmail.ru"],
        name: "Mail.ru",
        imap: "imap.mail.ru",
        smtp: "smtp.mail.ru",
        hint: "Mail.ru пускает внешние программы только по паролю приложения: \
               Настройки → Безопасность → Пароли для внешних приложений.",
    },
    Preset {
        domains: &["yandex.ru", "ya.ru", "yandex.com", "yandex.by", "yandex.kz", "narod.ru"],
        name: "Яндекс Почты",
        imap: "imap.yandex.ru",
        smtp: "smtp.yandex.ru",
        hint: "Нужен пароль приложения (id.yandex.ru → Безопасность → Пароли \
               приложений) и включённый IMAP в настройках почты.",
    },
    Preset {
        domains: &["gmail.com", "googlemail.com"],
        name: "Gmail",
        imap: "imap.gmail.com",
        smtp: "smtp.gmail.com",
        hint: "Gmail принимает только пароль приложения — он появляется после \
               включения двухэтапной аутентификации в аккаунте Google.",
    },
    // Международный gmx.com и немецкий GMX (.net/.de/.at/.ch) — разные
    // службы со своими серверами; учётка одной на серверах другой не входит.
    Preset {
        domains: &["gmx.com"],
        name: "GMX",
        imap: "imap.gmx.com",
        smtp: "mail.gmx.com",
        hint: "Доступ по IMAP в GMX выключен по умолчанию: включите его в \
               настройках ящика (POP3 & IMAP).",
    },
    Preset {
        domains: &["gmx.net", "gmx.de", "gmx.at", "gmx.ch"],
        name: "GMX",
        imap: "imap.gmx.net",
        smtp: "mail.gmx.net",
        hint: "Доступ по IMAP в GMX выключен по умолчанию: включите его в \
               настройках ящика (POP3 & IMAP).",
    },
    Preset {
        domains: &["rambler.ru", "lenta.ru", "autorambler.ru", "myrambler.ru", "ro.ru"],
        name: "Рамблер Почты",
        imap: "imap.rambler.ru",
        smtp: "smtp.rambler.ru",
        hint: "Включите доступ почтовым программам в настройках Рамблер Почты.",
    },
    Preset {
        domains: &["icloud.com", "me.com", "mac.com"],
        name: "iCloud",
        imap: "imap.mail.me.com",
        smtp: "smtp.mail.me.com",
        hint: "iCloud пускает только по паролю приложения: account.apple.com → \
               Вход и безопасность → Пароли приложений.",
    },
];

/// Серверы для адреса. `None` — адрес ещё не похож на адрес (нет домена с
/// точкой): угадывать по «user@ma» бессмысленно.
pub(crate) fn guess(email: &str) -> Option<Servers> {
    let domain = email.trim().rsplit_once('@')?.1.trim().to_ascii_lowercase();
    let (head, tld) = domain.rsplit_once('.')?;
    if head.is_empty() || tld.len() < 2 {
        return None;
    }
    if let Some(p) = PRESETS.iter().find(|p| p.domains.contains(&domain.as_str())) {
        // iCloud: SMTP только STARTTLS на 587.
        let smtp_port = if p.smtp == "smtp.mail.me.com" { 587 } else { 465 };
        return Some(Servers {
            imap_host: p.imap.to_string(),
            imap_port: 993,
            smtp_host: p.smtp.to_string(),
            smtp_port,
            provider: Some(p.name),
            password_hint: p.hint,
        });
    }
    Some(Servers {
        imap_host: format!("imap.{domain}"),
        imap_port: 993,
        smtp_host: format!("smtp.{domain}"),
        smtp_port: 465,
        provider: None,
        password_hint: "",
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn known_provider_by_any_of_its_domains() {
        let s = guess("someone@bk.ru").unwrap();
        assert_eq!(s.imap_host, "imap.mail.ru");
        assert_eq!(s.smtp_host, "smtp.mail.ru");
        assert_eq!((s.imap_port, s.smtp_port), (993, 465));
        assert_eq!(s.provider, Some("Mail.ru"));
        assert!(!s.password_hint.is_empty());
    }

    #[test]
    fn domain_is_case_and_space_insensitive() {
        let s = guess("  Someone@GMX.NET ").unwrap();
        assert_eq!(s.imap_host, "imap.gmx.net");
        assert_eq!(s.smtp_host, "mail.gmx.net");
        assert_eq!(guess("a@gmx.com").unwrap().imap_host, "imap.gmx.com");
    }

    #[test]
    fn icloud_smtp_is_starttls_587() {
        assert_eq!(guess("a@icloud.com").unwrap().smtp_port, 587);
    }

    #[test]
    fn unknown_domain_is_guessed_and_marked() {
        let s = guess("a@example.org").unwrap();
        assert_eq!(s.imap_host, "imap.example.org");
        assert_eq!(s.smtp_host, "smtp.example.org");
        assert_eq!(s.provider, None);
        assert_eq!(s.password_hint, "");
    }

    #[test]
    fn incomplete_address_gives_nothing() {
        for e in ["", "user", "user@", "user@mail", "user@mail.r", "user@.ru"] {
            assert_eq!(guess(e), None, "{e:?}");
        }
    }
}
