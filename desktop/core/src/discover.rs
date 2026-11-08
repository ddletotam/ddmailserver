//! Автонастройка подключения по адресу почты — режим «Автоматически» окна
//! подключения (контракт §5г-бис).
//!
//! Цепочка, по фазам; внутри фазы проверки идут параллельно, а победитель
//! выбирается по порядку кандидатов, а не по скорости ответа:
//!
//! 1. MX домена, сервер ddmail на `mail.<домен>` и `<домен>`
//!    (`/.well-known/ddmail`), автонастройка Thunderbird (домен, его
//!    `.well-known`, ISPDB).
//! 2. Сервер ddmail на MX-хостах. Нашёлся ddmail — дальше не ищем.
//! 3. IMAP: 993 (TLS) и 143 (STARTTLS) на кандидатах.
//! 4. SMTP: 465 (TLS), 587 (STARTTLS), затем 25 (STARTTLS).
//!
//! Пароль здесь не нужен и никуда не уходит: проверяется только, что на
//! порту живой сервер с рабочим TLS. Открытый текст не принимается — ни
//! IMAP без STARTTLS, ни `socketType=plain` из автонастройки.
//!
//! Прогресс считается от верхней границы числа проверок, известной до
//! первого запроса: несостоявшиеся проверки (MX меньше, чем мест, хосты
//! совпали) засчитываются сразу, и полоса доходит до конца при любом исходе.

use std::sync::Mutex;
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::time::Duration;

use futures::io::BufReader;
use futures::{AsyncBufReadExt, AsyncWriteExt};
use tokio_util::compat::TokioAsyncReadCompatExt;

/// Сколько ждём один ответ: TCP, рукопожатие и пару команд.
const PROBE_TIMEOUT: Duration = Duration::from_secs(8);
/// Отдельно — установка TCP: закрытый файрволом порт (443 на MX, 25 у
/// провайдера) молчит до таймаута, и ждать его все 8 с значит тянуть фазу.
const CONNECT_TIMEOUT: Duration = Duration::from_secs(3);
/// MX-хостов берём не больше этого (по приоритету).
const MAX_MX: usize = 2;
const MAX_IMAP: usize = 16;
const MAX_SMTP: usize = 27;
const AUTOCONFIG_URLS: u32 = 3;
const TOTAL_STEPS: u32 =
    1 + 2 + AUTOCONFIG_URLS + MAX_MX as u32 + MAX_IMAP as u32 + MAX_SMTP as u32;

/// Шифрование — по порту, как его выбирает само подключение
/// (`imap::open_tls`, `smtp::build_transport`): учётка хранит только порт.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Security {
    Tls,
    StartTls,
}

pub fn imap_security(port: u16) -> Security {
    if port == 143 { Security::StartTls } else { Security::Tls }
}

pub fn smtp_security(port: u16) -> Security {
    if matches!(port, 587 | 25 | 2525) { Security::StartTls } else { Security::Tls }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Endpoint {
    pub host: String,
    pub port: u16,
}

impl Endpoint {
    fn new(host: &str, port: u16) -> Self {
        Endpoint { host: host.to_string(), port }
    }
}

/// Серверы, которые клиент знает сам (таблица известных служб) — ставятся
/// в начало очереди кандидатов.
#[derive(Debug, Clone)]
pub struct Hint {
    pub imap_host: String,
    pub smtp_host: String,
    pub smtp_port: u16,
}

/// Что нашлось. Всё `None` — не нашлось ничего, пусть вводит руками.
#[derive(Debug, Clone, Default)]
pub struct Found {
    /// `https://<хост>` сервера ddmail — остальное тогда не ищется.
    pub ddmail_url: Option<String>,
    pub imap: Option<Endpoint>,
    pub smtp: Option<Endpoint>,
    /// Исходящий объявил AUTH. Нет — внутренний релей без авторизации.
    pub smtp_auth: bool,
    /// Логин из автонастройки, если он не email (например, часть до @).
    pub login: Option<String>,
    /// Название службы из автонастройки.
    pub provider: Option<String>,
}

#[derive(Debug, Clone)]
pub struct Progress {
    pub done: u32,
    pub total: u32,
    pub stage: String,
}

/// Домен адреса в ASCII (IDN → punycode), или `None`, если это не адрес.
pub fn email_domain(email: &str) -> Option<String> {
    let (local, domain) = email.trim().rsplit_once('@')?;
    let domain = domain.trim().trim_end_matches('.');
    if local.trim().is_empty() || !domain.contains('.') {
        return None;
    }
    let url = url::Url::parse(&format!("http://{domain}/")).ok()?;
    match url.host()? {
        url::Host::Domain(d) => Some(d.to_ascii_lowercase()),
        _ => None,
    }
}

struct Meter<'a> {
    done: AtomicU32,
    stage: Mutex<String>,
    cb: &'a dyn Fn(Progress),
}

impl Meter<'_> {
    fn emit(&self) {
        let stage = self.stage.lock().map(|s| s.clone()).unwrap_or_default();
        let done = self.done.load(Ordering::SeqCst).min(TOTAL_STEPS);
        (self.cb)(Progress { done, total: TOTAL_STEPS, stage });
    }
    fn stage(&self, s: impl Into<String>) {
        if let Ok(mut g) = self.stage.lock() {
            *g = s.into();
        }
        self.emit();
    }
    fn tick(&self, n: u32) {
        if n > 0 {
            self.done.fetch_add(n, Ordering::SeqCst);
            self.emit();
        }
    }
    fn finish(&self) {
        self.done.store(TOTAL_STEPS, Ordering::SeqCst);
        self.emit();
    }
}

/// Найти серверы для адреса. `cancel` проверяется между фазами: окно
/// закрыли — дальше не стучимся.
pub async fn discover(
    email: &str,
    hint: Option<Hint>,
    cancel: &AtomicBool,
    progress: &dyn Fn(Progress),
) -> Found {
    crate::tls::init();
    let meter = Meter { done: AtomicU32::new(0), stage: Mutex::new(String::new()), cb: progress };
    let mut found = Found::default();
    let Some(domain) = email_domain(email) else {
        meter.finish();
        return found;
    };
    let email = email.trim().to_string();
    let http = reqwest::Client::builder()
        .timeout(PROBE_TIMEOUT)
        .connect_timeout(CONNECT_TIMEOUT)
        .redirect(reqwest::redirect::Policy::limited(3))
        .build()
        .ok();

    // ── 1. MX, ddmail на домене, автонастройка ──
    meter.stage(format!("Ищем сервер для {domain}…"));
    let direct = [format!("mail.{domain}"), domain.clone()];
    let mx_fut = async {
        let r = mx_hosts(&domain).await;
        meter.tick(1);
        r
    };
    let auto_fut = autoconfig(http.as_ref(), &email, &domain, &meter);
    let dd_fut =
        first_in_order(direct.iter().map(|h| ddmail_probe(http.as_ref(), h)).collect(), &meter);
    tokio::pin!(mx_fut, auto_fut, dd_fut);
    // ddmail ответил — MX и автонастройку не ждём: они уже не нужны.
    let (mut mx, mut auto, mut dd_done) = (None, None, false);
    loop {
        tokio::select! {
            r = &mut dd_fut, if !dd_done => {
                dd_done = true;
                if let Some((i, ())) = r {
                    found.ddmail_url = Some(format!("https://{}", direct[i]));
                    meter.finish();
                    return found;
                }
            }
            r = &mut mx_fut, if mx.is_none() => mx = Some(r),
            r = &mut auto_fut, if auto.is_none() => auto = Some(r),
        }
        if dd_done && mx.is_some() && auto.is_some() {
            break;
        }
    }
    let mx = mx.unwrap_or_default();
    let auto = auto.flatten();
    if cancel.load(Ordering::SeqCst) {
        return found;
    }

    // ── 2. ddmail на MX ──
    let mx_new: Vec<&String> = mx.iter().filter(|h| !direct.contains(h)).collect();
    meter.tick((MAX_MX - mx_new.len()) as u32);
    if !mx_new.is_empty() {
        meter.stage("Проверяем почтовые серверы домена…");
        let dd_mx =
            first_in_order(mx_new.iter().map(|h| ddmail_probe(http.as_ref(), h)).collect(), &meter)
                .await;
        if let Some((i, ())) = dd_mx {
            found.ddmail_url = Some(format!("https://{}", mx_new[i]));
            meter.finish();
            return found;
        }
    }
    if cancel.load(Ordering::SeqCst) {
        return found;
    }
    if let Some(a) = &auto {
        found.login = a.login.clone();
        found.provider = a.name.clone();
    }

    // ── 3. IMAP ──
    meter.stage("Ищем входящий сервер (IMAP)…");
    let imap = imap_candidates(&domain, hint.as_ref(), auto.as_ref(), &mx);
    meter.tick((MAX_IMAP - imap.len()) as u32);
    let ok = first_in_order(
        imap.iter()
            .map(|e| async move {
                tokio::time::timeout(PROBE_TIMEOUT, probe_imap(&e.host, e.port))
                    .await
                    .ok()
                    .flatten()
            })
            .collect(),
        &meter,
    )
    .await;
    found.imap = ok.map(|(i, ())| imap[i].clone());
    if cancel.load(Ordering::SeqCst) {
        return found;
    }

    // ── 4. SMTP ──
    meter.stage("Ищем исходящий сервер (SMTP)…");
    let smtp = smtp_candidates(&domain, hint.as_ref(), auto.as_ref(), &mx, found.imap.as_ref());
    meter.tick((MAX_SMTP - smtp.len()) as u32);
    let res = first_in_order(
        smtp.iter()
            .map(|e| async move {
                tokio::time::timeout(PROBE_TIMEOUT, probe_smtp(&e.host, e.port))
                    .await
                    .ok()
                    .flatten()
            })
            .collect(),
        &meter,
    )
    .await;
    if let Some((i, auth)) = res {
        found.smtp = Some(smtp[i].clone());
        found.smtp_auth = auth;
    }
    meter.finish();
    found
}

/// Проверки идут разом, а победитель — первый успешный по порядку списка:
/// ждём только тех, кто стоит раньше него, остальные бросаем (их шаги
/// засчитываются сразу). Так порядок доверия соблюдён, а фаза длится столько,
/// сколько отвечает лучший живой кандидат, а не самый долгий таймаут.
async fn first_in_order<T, F>(futs: Vec<F>, meter: &Meter<'_>) -> Option<(usize, T)>
where
    F: std::future::Future<Output = Option<T>>,
{
    use futures::stream::{FuturesUnordered, StreamExt};
    let n = futs.len();
    let mut pending: FuturesUnordered<_> =
        futs.into_iter().enumerate().map(|(i, f)| async move { (i, f.await) }).collect();
    let mut results: Vec<Option<Option<T>>> = (0..n).map(|_| None).collect();
    let mut finished = 0;
    while let Some((i, r)) = pending.next().await {
        finished += 1;
        meter.tick(1);
        results[i] = Some(r);
        for j in 0..n {
            match &results[j] {
                None => break,
                Some(None) => {}
                Some(Some(_)) => {
                    meter.tick((n - finished) as u32);
                    return results[j].take().flatten().map(|t| (j, t));
                }
            }
        }
    }
    None
}

// ── Кандидаты ──

fn push_unique(v: &mut Vec<Endpoint>, e: Endpoint) {
    if !e.host.is_empty() && !v.contains(&e) {
        v.push(e);
    }
}

/// Известная служба по MX: домен на Яндекс 360 или VK WorkSpace ходит на их
/// серверы, а не на `imap.<домен>`.
fn mx_provider(mx: &str) -> Option<(&'static str, &'static str)> {
    let m = mx.to_ascii_lowercase();
    if m.ends_with(".yandex.net") || m.ends_with(".yandex.ru") {
        Some(("imap.yandex.ru", "smtp.yandex.ru"))
    } else if m.ends_with(".mail.ru") {
        Some(("imap.mail.ru", "smtp.mail.ru"))
    } else if m.ends_with(".google.com") || m.ends_with(".googlemail.com") {
        Some(("imap.gmail.com", "smtp.gmail.com"))
    } else {
        None
    }
}

/// Хосты по убыванию доверия: явные настройки (таблица, автонастройка,
/// служба по MX), затем догадки по домену, затем сами MX.
fn imap_candidates(
    domain: &str,
    hint: Option<&Hint>,
    auto: Option<&Autoconf>,
    mx: &[String],
) -> Vec<Endpoint> {
    let mut v = Vec::new();
    if let Some(e) = auto.and_then(|a| a.imap.clone()) {
        push_unique(&mut v, e);
    }
    let mut hosts: Vec<String> = Vec::new();
    if let Some(h) = hint {
        hosts.push(h.imap_host.clone());
    }
    hosts.extend(mx.iter().filter_map(|m| mx_provider(m)).map(|(i, _)| i.to_string()));
    hosts.push(format!("imap.{domain}"));
    hosts.push(format!("mail.{domain}"));
    hosts.push(domain.to_string());
    hosts.extend(mx.iter().cloned());
    for h in hosts {
        push_unique(&mut v, Endpoint::new(&h, 993));
        push_unique(&mut v, Endpoint::new(&h, 143));
    }
    v.truncate(MAX_IMAP);
    v
}

/// 465 и 587 на всех хостах раньше любого 25: на 25 отвечает и приём почты
/// (MX), а он чужие письма наружу не повезёт. Поэтому 25 на MX-хостах не
/// пробуем вовсе — только на прочих (внутренний релей).
fn smtp_candidates(
    domain: &str,
    hint: Option<&Hint>,
    auto: Option<&Autoconf>,
    mx: &[String],
    imap: Option<&Endpoint>,
) -> Vec<Endpoint> {
    let mut v = Vec::new();
    if let Some(e) = auto.and_then(|a| a.smtp.clone()) {
        push_unique(&mut v, e);
    }
    if let Some(h) = hint {
        push_unique(&mut v, Endpoint::new(&h.smtp_host, h.smtp_port));
    }
    let mut hosts: Vec<String> = Vec::new();
    hosts.extend(mx.iter().filter_map(|m| mx_provider(m)).map(|(_, s)| s.to_string()));
    hosts.push(format!("smtp.{domain}"));
    if let Some(i) = imap {
        hosts.push(i.host.clone());
    }
    hosts.push(format!("mail.{domain}"));
    hosts.push(domain.to_string());
    hosts.extend(mx.iter().cloned());
    let mut uniq: Vec<String> = Vec::new();
    for h in hosts {
        if !uniq.contains(&h) {
            uniq.push(h);
        }
    }
    for h in &uniq {
        push_unique(&mut v, Endpoint::new(h, 465));
        push_unique(&mut v, Endpoint::new(h, 587));
    }
    for h in uniq.iter().filter(|h| !mx.contains(h)) {
        push_unique(&mut v, Endpoint::new(h, 25));
    }
    v.truncate(MAX_SMTP);
    v
}

// ── DNS и HTTP ──

async fn mx_hosts(domain: &str) -> Vec<String> {
    let Ok(resolver) = hickory_resolver::TokioAsyncResolver::tokio_from_system_conf() else {
        return Vec::new();
    };
    let Ok(Ok(lookup)) = tokio::time::timeout(PROBE_TIMEOUT, resolver.mx_lookup(domain)).await
    else {
        return Vec::new();
    };
    let mut v: Vec<(u16, String)> = lookup
        .iter()
        .map(|m| {
            (m.preference(), m.exchange().to_ascii().trim_end_matches('.').to_ascii_lowercase())
        })
        // Null MX (RFC 7505): «почту не принимаем».
        .filter(|(_, h)| !h.is_empty())
        .collect();
    v.sort();
    let mut out: Vec<String> = Vec::new();
    for (_, h) in v {
        if !out.contains(&h) {
            out.push(h);
        }
    }
    out.truncate(MAX_MX);
    out
}

async fn ddmail_probe(http: Option<&reqwest::Client>, host: &str) -> Option<()> {
    is_ddmail(http, host).await.then_some(())
}

async fn is_ddmail(http: Option<&reqwest::Client>, host: &str) -> bool {
    let Some(http) = http else { return false };
    let Ok(resp) = http.get(format!("https://{host}/.well-known/ddmail")).send().await else {
        return false;
    };
    if !resp.status().is_success() {
        return false;
    }
    resp.json::<serde_json::Value>()
        .await
        .ok()
        .and_then(|v| v.get("ddmail").and_then(|d| d.as_bool()))
        .unwrap_or(false)
}

#[derive(Debug, Clone, Default, PartialEq)]
struct Autoconf {
    imap: Option<Endpoint>,
    smtp: Option<Endpoint>,
    login: Option<String>,
    name: Option<String>,
}

/// Только https: по http подменённый ответ увёл бы пароль на чужой хост.
/// Первая по порядку годная выдача; ISPDB Thunderbird — последней.
async fn autoconfig(
    http: Option<&reqwest::Client>,
    email: &str,
    domain: &str,
    meter: &Meter<'_>,
) -> Option<Autoconf> {
    let urls = [
        format!(
            "https://autoconfig.{domain}/mail/config-v1.1.xml?emailaddress={}",
            urlencoding::encode(email)
        ),
        format!("https://{domain}/.well-known/autoconfig/mail/config-v1.1.xml"),
        format!("https://autoconfig.thunderbird.net/v1.1/{domain}"),
    ];
    let futs = urls
        .iter()
        .map(|u| async move {
            let resp = http?.get(u).send().await.ok()?;
            if !resp.status().is_success() {
                return None;
            }
            parse_autoconfig(&resp.text().await.ok()?, email)
                .filter(|a| a.imap.is_some() || a.smtp.is_some())
        })
        .collect();
    first_in_order(futs, meter).await.map(|(_, a)| a)
}

fn expand(tpl: &str, email: &str) -> String {
    let (local, domain) = email.rsplit_once('@').unwrap_or((email, ""));
    tpl.replace("%EMAILADDRESS%", email)
        .replace("%EMAILLOCALPART%", local)
        .replace("%EMAILDOMAIN%", domain)
}

fn parse_autoconfig(xml: &str, email: &str) -> Option<Autoconf> {
    let doc = roxmltree::Document::parse(xml).ok()?;
    let child = |n: roxmltree::Node, tag: &str| {
        n.children()
            .find(|c| c.has_tag_name(tag))
            .and_then(|c| c.text())
            .map(|t| t.trim().to_string())
    };
    // Сервер годится, если его шифрование совпадает с тем, что подключение
    // выберет по порту, — иначе сохранённая учётка не подключится.
    let pick = |tag: &str, kind: &str, rule: fn(u16) -> Security| {
        doc.descendants()
            .filter(|n| n.has_tag_name(tag) && n.attribute("type") == Some(kind))
            .find_map(|n| {
                let host = expand(&child(n, "hostname")?, email).to_ascii_lowercase();
                let port: u16 = child(n, "port")?.parse().ok()?;
                let sec = match child(n, "socketType")?.to_ascii_uppercase().as_str() {
                    "SSL" | "TLS" => Security::Tls,
                    "STARTTLS" => Security::StartTls,
                    _ => return None,
                };
                (rule(port) == sec && !host.is_empty())
                    .then(|| (Endpoint { host, port }, child(n, "username")))
            })
    };
    let imap = pick("incomingServer", "imap", imap_security);
    let smtp = pick("outgoingServer", "smtp", smtp_security);
    let login = imap
        .as_ref()
        .and_then(|(_, u)| u.as_deref())
        .map(|u| expand(u, email))
        .filter(|u| !u.is_empty() && u != email);
    let name = doc
        .descendants()
        .find(|n| n.has_tag_name("displayShortName") || n.has_tag_name("displayName"))
        .and_then(|n| n.text())
        .map(|t| t.trim().to_string())
        .filter(|t| !t.is_empty());
    Some(Autoconf { imap: imap.map(|(e, _)| e), smtp: smtp.map(|(e, _)| e), login, name })
}

// ── Проверки портов ──

async fn tcp(host: &str, port: u16) -> Option<tokio_util::compat::Compat<tokio::net::TcpStream>> {
    tokio::time::timeout(CONNECT_TIMEOUT, tokio::net::TcpStream::connect((host, port)))
        .await
        .ok()?
        .ok()
        .map(|s| s.compat())
}

async fn tls_over(
    host: &str,
    s: tokio_util::compat::Compat<tokio::net::TcpStream>,
) -> Option<crate::imap::TlsStream> {
    let name = crate::tls::server_name(host).ok()?;
    crate::tls::connector().connect(name, s).await.ok()
}

async fn read_line<R: futures::AsyncBufRead + Unpin>(r: &mut R) -> Option<String> {
    let mut line = String::new();
    let n = r.read_line(&mut line).await.ok()?;
    (n > 0).then_some(line)
}

async fn send<W: futures::AsyncWrite + Unpin>(w: &mut W, cmd: &str) -> Option<()> {
    w.write_all(cmd.as_bytes()).await.ok()?;
    w.flush().await.ok()
}

/// Строки до ответа с тегом `tag`; `Some`, если он `OK`.
async fn imap_tagged_ok<R>(r: &mut R, tag: &str) -> Option<()>
where
    R: futures::AsyncBufRead + Unpin,
{
    let prefix = format!("{tag} ");
    loop {
        let line = read_line(r).await?;
        if let Some(rest) = line.strip_prefix(&prefix) {
            return rest.to_ascii_uppercase().starts_with("OK").then_some(());
        }
    }
}

fn imap_greeting_ok(line: &str) -> bool {
    let u = line.to_ascii_uppercase();
    u.starts_with("* OK") || u.starts_with("* PREAUTH")
}

/// Живой IMAP с TLS: 993 — TLS сразу, 143 — STARTTLS и NOOP уже под TLS.
async fn probe_imap(host: &str, port: u16) -> Option<()> {
    let s = tcp(host, port).await?;
    let tls = if imap_security(port) == Security::StartTls {
        let mut r = BufReader::new(s);
        if !imap_greeting_ok(&read_line(&mut r).await?) {
            return None;
        }
        send(&mut r, "a1 STARTTLS\r\n").await?;
        imap_tagged_ok(&mut r, "a1").await?;
        let tls = tls_over(host, r.into_inner()).await?;
        let mut r = BufReader::new(tls);
        send(&mut r, "a2 NOOP\r\n").await?;
        imap_tagged_ok(&mut r, "a2").await?;
        r
    } else {
        let tls = tls_over(host, s).await?;
        let mut r = BufReader::new(tls);
        if !imap_greeting_ok(&read_line(&mut r).await?) {
            return None;
        }
        r
    };
    let mut r = tls;
    let _ = send(&mut r, "a9 LOGOUT\r\n").await;
    Some(())
}

/// Ответ SMTP целиком (многострочный `250-…`): код и тексты строк.
async fn smtp_reply<R: futures::AsyncBufRead + Unpin>(r: &mut R) -> Option<(u16, Vec<String>)> {
    let mut lines = Vec::new();
    loop {
        let line = read_line(r).await?;
        let code: u16 = line.get(..3)?.parse().ok()?;
        lines.push(line.get(4..).unwrap_or("").trim().to_string());
        if line.as_bytes().get(3) != Some(&b'-') {
            return Some((code, lines));
        }
    }
}

async fn smtp_ehlo<S>(r: &mut S) -> Option<Vec<String>>
where
    S: futures::AsyncBufRead + futures::AsyncWrite + Unpin,
{
    send(r, "EHLO localhost\r\n").await?;
    let (code, caps) = smtp_reply(r).await?;
    (code == 250).then_some(caps)
}

fn smtp_has_auth(caps: &[String]) -> bool {
    caps.iter().any(|c| c.to_ascii_uppercase().starts_with("AUTH"))
}

/// Живой SMTP с TLS. `Some(auth)` — объявлен ли AUTH уже под TLS (до него
/// многие серверы AUTH не показывают намеренно).
async fn probe_smtp(host: &str, port: u16) -> Option<bool> {
    let s = tcp(host, port).await?;
    let mut r = if smtp_security(port) == Security::StartTls {
        let mut r = BufReader::new(s);
        if smtp_reply(&mut r).await?.0 != 220 {
            return None;
        }
        let caps = smtp_ehlo(&mut r).await?;
        if !caps.iter().any(|c| c.eq_ignore_ascii_case("STARTTLS")) {
            return None;
        }
        send(&mut r, "STARTTLS\r\n").await?;
        if smtp_reply(&mut r).await?.0 != 220 {
            return None;
        }
        BufReader::new(tls_over(host, r.into_inner()).await?)
    } else {
        let mut r = BufReader::new(tls_over(host, s).await?);
        if smtp_reply(&mut r).await?.0 != 220 {
            return None;
        }
        r
    };
    let caps = smtp_ehlo(&mut r).await?;
    let _ = send(&mut r, "QUIT\r\n").await;
    Some(smtp_has_auth(&caps))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn domain_of_address() {
        assert_eq!(email_domain(" Me@Example.ORG ").as_deref(), Some("example.org"));
        assert_eq!(email_domain("a@почта.рф").as_deref(), Some("xn--80a1acny.xn--p1ai"));
        for bad in ["", "a", "a@", "@x.ru", "a@localhost"] {
            assert_eq!(email_domain(bad), None, "{bad:?}");
        }
    }

    #[test]
    fn security_follows_port() {
        assert_eq!(imap_security(143), Security::StartTls);
        assert_eq!(imap_security(993), Security::Tls);
        assert_eq!(smtp_security(587), Security::StartTls);
        assert_eq!(smtp_security(25), Security::StartTls);
        assert_eq!(smtp_security(465), Security::Tls);
    }

    const XML: &str = r#"<?xml version="1.0"?>
<clientConfig version="1.1">
  <emailProvider id="example.org">
    <domain>example.org</domain>
    <displayName>Example Mail</displayName>
    <incomingServer type="pop3">
      <hostname>pop.example.org</hostname><port>995</port><socketType>SSL</socketType>
    </incomingServer>
    <incomingServer type="imap">
      <hostname>plain.example.org</hostname><port>143</port><socketType>plain</socketType>
    </incomingServer>
    <incomingServer type="imap">
      <hostname>imap.%EMAILDOMAIN%</hostname><port>993</port><socketType>SSL</socketType>
      <username>%EMAILLOCALPART%</username>
    </incomingServer>
    <outgoingServer type="smtp">
      <hostname>smtp.example.org</hostname><port>587</port><socketType>STARTTLS</socketType>
      <username>%EMAILADDRESS%</username>
    </outgoingServer>
  </emailProvider>
</clientConfig>"#;

    #[test]
    fn autoconfig_takes_encrypted_imap_and_expands_placeholders() {
        let a = parse_autoconfig(XML, "bob@example.org").unwrap();
        assert_eq!(a.imap, Some(Endpoint::new("imap.example.org", 993)));
        assert_eq!(a.smtp, Some(Endpoint::new("smtp.example.org", 587)));
        assert_eq!(a.login.as_deref(), Some("bob"));
        assert_eq!(a.name.as_deref(), Some("Example Mail"));
    }

    #[test]
    fn autoconfig_rejects_security_that_port_would_not_use() {
        // SSL на 587: подключение по порту пошло бы STARTTLS — не сойдётся.
        let xml =
            XML.replace("<port>587</port><socketType>STARTTLS", "<port>587</port><socketType>SSL");
        let a = parse_autoconfig(&xml, "bob@example.org").unwrap();
        assert_eq!(a.smtp, None);
        assert!(parse_autoconfig("<not xml", "a@b.c").is_none());
    }

    #[test]
    fn imap_candidates_put_known_first_and_cap() {
        let auto =
            Autoconf { imap: Some(Endpoint::new("in.example.org", 993)), ..Default::default() };
        let mx = vec!["mx.yandex.net".to_string(), "mx2.example.org".to_string()];
        let v = imap_candidates("example.org", None, Some(&auto), &mx);
        assert_eq!(v[0], Endpoint::new("in.example.org", 993));
        assert_eq!(v[1], Endpoint::new("imap.yandex.ru", 993));
        assert_eq!(v[2], Endpoint::new("imap.yandex.ru", 143));
        assert_eq!(v[3], Endpoint::new("imap.example.org", 993));
        assert!(v.contains(&Endpoint::new("mx2.example.org", 143)));
        assert!(v.len() <= MAX_IMAP);
    }

    #[test]
    fn smtp_port_25_comes_last_and_never_on_mx() {
        let mx = vec!["mail.example.org".to_string()];
        let v = smtp_candidates("example.org", None, None, &mx, None);
        let first25 = v.iter().position(|e| e.port == 25).unwrap();
        assert!(v[..first25].iter().all(|e| e.port != 25));
        assert!(v[first25..].iter().all(|e| e.port == 25));
        assert!(!v.contains(&Endpoint::new("mail.example.org", 25)));
        assert!(v.contains(&Endpoint::new("mail.example.org", 465)));
        assert!(v.len() <= MAX_SMTP);
    }

    #[test]
    fn worst_case_candidates_fit_the_budget() {
        let hint = Hint { imap_host: "i.x.ru".into(), smtp_host: "s.x.ru".into(), smtp_port: 465 };
        let auto = Autoconf {
            imap: Some(Endpoint::new("a.x.ru", 993)),
            smtp: Some(Endpoint::new("b.x.ru", 587)),
            ..Default::default()
        };
        let mx = vec!["mx.yandex.net".to_string(), "aspmx.l.google.com".to_string()];
        let imap = imap_candidates("x.ru", Some(&hint), Some(&auto), &mx);
        let smtp = smtp_candidates(
            "x.ru",
            Some(&hint),
            Some(&auto),
            &mx,
            Some(&Endpoint::new("q.x.ru", 993)),
        );
        assert!(imap.len() <= MAX_IMAP && smtp.len() <= MAX_SMTP);
        // Явные настройки не срезаются лимитом.
        assert_eq!(imap[0].host, "a.x.ru");
        assert_eq!(smtp[0].host, "b.x.ru");
        assert_eq!(smtp[1].host, "s.x.ru");
    }

    #[tokio::test]
    async fn first_in_order_waits_only_for_earlier_candidates() {
        let cb = |_: Progress| {};
        let meter = Meter { done: AtomicU32::new(0), stage: Mutex::new(String::new()), cb: &cb };
        let slow = |ms: u64, ok: bool| async move {
            tokio::time::sleep(Duration::from_millis(ms)).await;
            ok.then_some(ms)
        };
        // Второй успешен быстро, но первый (провальный) старше — ждём его;
        // третий висит долго, его не ждём.
        let t = std::time::Instant::now();
        let futs: Vec<std::pin::Pin<Box<dyn std::future::Future<Output = Option<u64>>>>> =
            vec![Box::pin(slow(50, false)), Box::pin(slow(10, true)), Box::pin(slow(5_000, true))];
        assert_eq!(first_in_order(futs, &meter).await, Some((1, 10)));
        assert!(t.elapsed() < Duration::from_secs(1));
        assert_eq!(meter.done.load(Ordering::SeqCst), 3);

        let futs: Vec<std::pin::Pin<Box<dyn std::future::Future<Output = Option<u64>>>>> =
            vec![Box::pin(slow(30, true)), Box::pin(slow(1, true))];
        assert_eq!(first_in_order(futs, &meter).await, Some((0, 30)));
    }

    #[test]
    fn smtp_capabilities() {
        assert!(smtp_has_auth(&["mx.example.org".into(), "AUTH PLAIN LOGIN".into()]));
        assert!(!smtp_has_auth(&["mx.example.org".into(), "STARTTLS".into()]));
    }

    #[tokio::test]
    async fn smtp_multiline_reply() {
        let data = b"250-mx.example.org\r\n250-AUTH PLAIN\r\n250 SIZE 100\r\n".to_vec();
        let mut r = BufReader::new(futures::io::Cursor::new(data));
        let (code, lines) = smtp_reply(&mut r).await.unwrap();
        assert_eq!(code, 250);
        assert_eq!(lines, vec!["mx.example.org", "AUTH PLAIN", "SIZE 100"]);
    }
}
