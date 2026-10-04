//! An HTTP [`Resources`](crate::Resources) implementation — **only** when the
//! `net` feature is on.
//!
//! The renderer proper never opens a socket, and that stays true: this module
//! is not reachable from `render()`, it is a resolver a host may choose to pass
//! to `render_with()`. It lives here rather than in each host because both the
//! client and the harness want the same thing — a bounded, cached, parallel
//! prefetch — and two copies of that drift.
//!
//! Two gates, both enforced here and nowhere else:
//!
//! * **Whose permission.** Loading an image tells its sender the message was
//!   opened, so the host passes a predicate over hosts ([`HttpResources::prefetch`])
//!   and nothing it rejects is ever requested — not the first hop, not a
//!   redirect target. The URLs it is asked about are read out of the same
//!   html5ever DOM layout reads from, so an attribute a regex upstream missed
//!   (unquoted, entity-encoded, in a `style`) cannot slip past it: what is
//!   checked is byte-for-byte what would be fetched.
//! * **Where it may go.** A mail is attacker-controlled input, so a URL in it
//!   must not reach the user's own network: loopback, RFC 1918, link-local,
//!   CGNAT, ULA and the rest are refused ([`is_public_ip`]) after resolution,
//!   and the connection goes to exactly the addresses that were checked (see
//!   [`PinnedResolver`]) — a second, different DNS answer cannot sneak in
//!   between the check and the connect.

use std::collections::HashMap;
use std::io::Read;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr, ToSocketAddrs};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use reqwest::dns::{Addrs, Name, Resolve, Resolving};
use reqwest::Url;

use crate::dom::{attr, children, collect_style_text, is_dropped, parse, tag};
use crate::Resources;

/// Per-message ceilings. A mail past any of them renders with placeholders for
/// the remainder rather than holding the panel hostage.
const MAX_IMAGES: usize = 64;
const MAX_BYTES: usize = 8 * 1024 * 1024;
const MAX_REDIRECTS: usize = 4;
const REQUEST_TIMEOUT: Duration = Duration::from_secs(6);
const BATCH_DEADLINE: Duration = Duration::from_secs(10);
const WORKERS: usize = 6;

/// Process-wide cache budget. One sender's logo repeats in every message they
/// send, and a re-layout (a resized panel) must not re-fetch the world.
const CACHE_BUDGET: usize = 128 * 1024 * 1024;

struct Cache {
    entries: HashMap<String, Option<Arc<Vec<u8>>>>,
    bytes: usize,
}

fn cache() -> &'static Mutex<Cache> {
    static CACHE: OnceLock<Mutex<Cache>> = OnceLock::new();
    CACHE.get_or_init(|| Mutex::new(Cache { entries: HashMap::new(), bytes: 0 }))
}

/// Host → the addresses [`vet`] resolved and approved for it. The only source
/// of answers [`PinnedResolver`] gives the HTTP client.
fn pins() -> &'static Mutex<HashMap<String, Vec<SocketAddr>>> {
    static PINS: OnceLock<Mutex<HashMap<String, Vec<SocketAddr>>>> = OnceLock::new();
    PINS.get_or_init(|| Mutex::new(HashMap::new()))
}

/// The client's DNS: never looks anything up itself, only hands back what
/// [`vet`] already resolved and checked. A host nobody vetted fails to
/// connect — so does the DNS-rebinding trick of answering the check with a
/// public address and the connect with `127.0.0.1`, because there is no
/// second lookup.
struct PinnedResolver;

impl Resolve for PinnedResolver {
    fn resolve(&self, name: Name) -> Resolving {
        let key = name.as_str().trim_end_matches('.').to_ascii_lowercase();
        let addrs = pins().lock().unwrap_or_else(|p| p.into_inner()).get(&key).cloned();
        Box::pin(async move {
            match addrs {
                Some(v) if !v.is_empty() => Ok(Box::new(v.into_iter()) as Addrs),
                _ => Err(format!("{key}: host was not vetted").into()),
            }
        })
    }
}

fn client() -> &'static reqwest::blocking::Client {
    static CLIENT: OnceLock<reqwest::blocking::Client> = OnceLock::new();
    CLIENT.get_or_init(|| {
        reqwest::blocking::Client::builder()
            .timeout(REQUEST_TIMEOUT)
            // Redirects are followed by hand in `get`: every hop has to pass
            // the sender's permission and the address check again.
            .redirect(reqwest::redirect::Policy::none())
            // A proxy resolves the name itself, out of reach of the address
            // check — and an intranet proxy is exactly what the check is for.
            .no_proxy()
            .dns_resolver(Arc::new(PinnedResolver))
            // Some CDNs answer an empty agent with a 403.
            .user_agent("Mozilla/5.0 (compatible; ddmail)")
            .build()
            .unwrap_or_default()
    })
}

/// Everything fetched for one message, keyed by the `src` as layout sees it.
pub struct HttpResources(HashMap<String, Arc<Vec<u8>>>);

impl Resources for HttpResources {
    fn fetch(&self, src: &str) -> Option<Vec<u8>> {
        self.0.get(src.trim()).map(|b| b.as_ref().clone())
    }
}

impl HttpResources {
    /// Fetch every remote image the document would paint — `<img src>` and
    /// CSS `background` URLs — whose host `allow_host` accepts, in parallel,
    /// then hand the result to `render_with`.
    ///
    /// `allow_host` gets the URL's host as the URL parser reads it: lowercase,
    /// IDNA-encoded, an IPv6 literal in brackets, no port. It is asked again
    /// for every redirect target. A host that says no is never contacted.
    ///
    /// A *pre*-fetch rather than a lazy callback for two reasons: layout asks
    /// for images one at a time on one thread, so a newsletter with thirty of
    /// them would serialise thirty round trips into a visibly stalled bubble;
    /// and a deadline for the batch is only enforceable if something owns the
    /// batch.
    pub fn prefetch(html: &str, allow_host: &(dyn Fn(&str) -> bool + Sync)) -> Self {
        let wanted = plan(html, allow_host);

        let mut out: HashMap<String, Arc<Vec<u8>>> = HashMap::new();
        let mut todo: Vec<(String, Url)> = Vec::new();
        {
            let cache = cache().lock().unwrap_or_else(|p| p.into_inner());
            for (src, url) in wanted {
                match cache.entries.get(&src) {
                    Some(Some(bytes)) => {
                        out.insert(src, Arc::clone(bytes));
                    }
                    Some(None) => {} // known-bad; asking again costs a timeout
                    None => todo.push((src, url)),
                }
            }
        }
        if todo.is_empty() {
            return HttpResources(out);
        }

        let deadline = Instant::now() + BATCH_DEADLINE;
        let queue = Mutex::new(todo.into_iter());
        let done: Mutex<Vec<(String, Option<Vec<u8>>)>> = Mutex::new(Vec::new());
        std::thread::scope(|scope| {
            for _ in 0..WORKERS {
                scope.spawn(|| loop {
                    if Instant::now() >= deadline {
                        return;
                    }
                    let next = queue.lock().unwrap_or_else(|p| p.into_inner()).next();
                    let Some((src, url)) = next else { return };
                    let bytes = get(url, allow_host);
                    done.lock().unwrap_or_else(|p| p.into_inner()).push((src, bytes));
                });
            }
        });

        let fetched = done.into_inner().unwrap_or_else(|p| p.into_inner());
        let mut cache = cache().lock().unwrap_or_else(|p| p.into_inner());
        if cache.bytes > CACHE_BUDGET {
            // Not an LRU: a flat clear is one line, and the cost of guessing
            // wrong is re-fetching a logo, not a stall.
            cache.entries.clear();
            cache.bytes = 0;
        }
        for (src, bytes) in fetched {
            let entry = bytes.map(Arc::new);
            if let Some(b) = &entry {
                cache.bytes += b.len();
                out.insert(src.clone(), Arc::clone(b));
            }
            cache.entries.insert(src, entry);
        }
        HttpResources(out)
    }
}

impl HttpResources {
    /// What [`prefetch`](Self::prefetch) can hand over right now without
    /// touching the network: this document's allowed images that are already
    /// in the in-memory cache, and whether that is all of them (`true`) or
    /// `prefetch` would still have to go out for some (`false`).
    ///
    /// Lets a host paint a mail at once — with placeholders for what is not
    /// here yet — and fetch the rest off its paint path. Same plan and same
    /// gate as `prefetch`; a URL already known to fail counts as resolved,
    /// exactly as `prefetch` would not ask for it again.
    pub fn cached(html: &str, allow_host: &(dyn Fn(&str) -> bool + Sync)) -> (Self, bool) {
        let wanted = plan(html, allow_host);
        let mut out: HashMap<String, Arc<Vec<u8>>> = HashMap::new();
        let mut complete = true;
        let cache = cache().lock().unwrap_or_else(|p| p.into_inner());
        for (src, _) in wanted {
            match cache.entries.get(&src) {
                Some(Some(bytes)) => {
                    out.insert(src, Arc::clone(bytes));
                }
                Some(None) => {}
                None => complete = false,
            }
        }
        (HttpResources(out), complete)
    }
}

/// What `prefetch` will download for `html`: the remote images layout would
/// ask for, minus everything `allow_host` rejects, deduplicated and capped.
/// Keyed by the `src` exactly as layout will pass it to [`Resources::fetch`].
fn plan(html: &str, allow_host: &dyn Fn(&str) -> bool) -> Vec<(String, Url)> {
    let mut out: Vec<(String, Url)> = Vec::new();
    for src in image_srcs(html) {
        if out.len() >= MAX_IMAGES {
            break;
        }
        if out.iter().any(|(s, _)| *s == src) {
            continue;
        }
        let Some(url) = remote_url(&src) else { continue };
        if url.host_str().is_some_and(allow_host) {
            out.push((src, url));
        }
    }
    out
}

/// One request chain: the URL, then up to [`MAX_REDIRECTS`] redirects, each
/// hop re-checked against both gates before anything is sent.
fn get(mut url: Url, allow_host: &dyn Fn(&str) -> bool) -> Option<Vec<u8>> {
    // One budget for the whole chain, as when the client followed redirects
    // itself: five hops must not each get a fresh timeout.
    let until = Instant::now() + REQUEST_TIMEOUT;
    for _ in 0..=MAX_REDIRECTS {
        if Instant::now() >= until {
            return None;
        }
        if !matches!(url.scheme(), "http" | "https") || !url.host_str().is_some_and(allow_host) {
            return None;
        }
        vet(&url)?;
        let mut resp = client().get(url.clone()).send().ok()?;
        if resp.status().is_redirection() {
            let next = resp.headers().get(reqwest::header::LOCATION)?.to_str().ok()?;
            url = url.join(next).ok()?;
            continue;
        }
        if !resp.status().is_success() {
            return None;
        }
        // The header is a cheap early no; the body is still read through a
        // hard cap, because a chunked response has no header to lie with.
        if resp.content_length().is_some_and(|n| n > MAX_BYTES as u64) {
            return None;
        }
        let mut body = Vec::new();
        (&mut resp).take(MAX_BYTES as u64 + 1).read_to_end(&mut body).ok()?;
        if body.is_empty() || body.len() > MAX_BYTES {
            return None;
        }
        return Some(body);
    }
    None
}

/// Resolve `url`'s host and accept it only if **every** address is public;
/// then pin those addresses for [`PinnedResolver`]. An IP literal never
/// reaches a resolver, so it is checked as is.
fn vet(url: &Url) -> Option<()> {
    match url.host()? {
        url::Host::Ipv4(ip) => is_public_ip(IpAddr::V4(ip)).then_some(()),
        url::Host::Ipv6(ip) => is_public_ip(IpAddr::V6(ip)).then_some(()),
        url::Host::Domain(d) => {
            let port = url.port_or_known_default()?;
            let addrs: Vec<SocketAddr> = (d, port).to_socket_addrs().ok()?.collect();
            if addrs.is_empty() || !addrs.iter().all(|a| is_public_ip(a.ip())) {
                return None;
            }
            let key = d.trim_end_matches('.').to_ascii_lowercase();
            pins().lock().unwrap_or_else(|p| p.into_inner()).insert(key, addrs);
            Some(())
        }
    }
}

/// Whether `ip` is an ordinary address on the public internet. Everything that
/// can name the user's own machine or network — or that has no business being
/// a web server — is not.
pub fn is_public_ip(ip: IpAddr) -> bool {
    match ip {
        IpAddr::V4(v4) => is_public_v4(v4),
        IpAddr::V6(v6) => is_public_v6(v6),
    }
}

fn is_public_v4(ip: Ipv4Addr) -> bool {
    let [a, b, c, _] = ip.octets();
    let private = a == 0 // "this network", incl. 0.0.0.0
        || a == 10
        || a == 127
        || (a == 100 && (64..128).contains(&b)) // CGNAT 100.64/10
        || (a == 169 && b == 254) // link-local, cloud metadata
        || (a == 172 && (16..32).contains(&b))
        || (a == 192 && b == 168)
        || (a == 192 && b == 0 && c == 0) // IETF protocol assignments
        || (a == 192 && b == 0 && c == 2) // TEST-NET-1
        || (a == 192 && b == 88 && c == 99) // 6to4 relay anycast
        || (a == 198 && (b == 18 || b == 19)) // benchmarking
        || (a == 198 && b == 51 && c == 100) // TEST-NET-2
        || (a == 203 && b == 0 && c == 113) // TEST-NET-3
        || a >= 224; // multicast, reserved, broadcast
    !private
}

fn is_public_v6(ip: Ipv6Addr) -> bool {
    let s = ip.segments();
    // Forms that carry an IPv4 address inside: judge the address they carry.
    if let Some(v4) = ip.to_ipv4_mapped() {
        return is_public_v4(v4); // ::ffff:a.b.c.d
    }
    if s[0] == 0x64 && s[1] == 0xff9b && s[2..6] == [0, 0, 0, 0] {
        return is_public_v4(embedded_v4(s[6], s[7])); // NAT64 64:ff9b::/96
    }
    if s[0] == 0x2002 {
        return is_public_v4(embedded_v4(s[1], s[2])); // 6to4 2002::/16
    }
    // Only global unicast 2000::/3 is public at all; that alone excludes ::,
    // ::1, IPv4-compatible ::a.b.c.d, ULA fc00::/7, link-local fe80::/10,
    // site-local fec0::/10, multicast ff00::/8 and the 64:ff9b:1::/48 range.
    if s[0] & 0xe000 != 0x2000 {
        return false;
    }
    // 2001::/23 is IETF protocol assignments — Teredo (which embeds an IPv4
    // peer), ORCHID, benchmarking; 2001:db8::/32 is documentation.
    let ietf = s[0] == 0x2001 && s[1] < 0x0200;
    let doc = s[0] == 0x2001 && s[1] == 0x0db8;
    !(ietf || doc)
}

fn embedded_v4(hi: u16, lo: u16) -> Ipv4Addr {
    Ipv4Addr::new((hi >> 8) as u8, hi as u8, (lo >> 8) as u8, lo as u8)
}

/// A host from an allow-list, spelled the way [`HttpResources::prefetch`]
/// hands hosts to its gate: lowercase, IDNA-encoded, no port, no userinfo.
/// `None` for anything that does not parse as a host.
pub fn normalize_host(host: &str) -> Option<String> {
    let host = host.trim();
    if host.is_empty() || host.contains(['/', '\\', '?', '#']) {
        return None;
    }
    Url::parse(&format!("https://{host}/")).ok()?.host_str().map(str::to_owned)
}

/// `src` as a fetchable URL, or `None` for anything that is not plain remote
/// HTTP(S). `//cdn.example/x.png` is a real thing in mail.
fn remote_url(src: &str) -> Option<Url> {
    let src = src.trim();
    let abs = if starts_ci(src, "http://") || starts_ci(src, "https://") {
        src.to_string()
    } else if src.starts_with("//") {
        format!("https:{src}")
    } else {
        return None;
    };
    let url = Url::parse(&abs).ok()?;
    (matches!(url.scheme(), "http" | "https") && url.host_str().is_some()).then_some(url)
}

fn starts_ci(s: &str, prefix: &str) -> bool {
    s.len() >= prefix.len() && s.as_bytes()[..prefix.len()].eq_ignore_ascii_case(prefix.as_bytes())
}

/// Every image URL layout could ask [`Resources::fetch`] for, in document
/// order: `<img src>` and the `url(...)` of `background`/`background-image`
/// declarations in `<style>` and `style=""`. Read from the html5ever DOM, the
/// same tree and the same attribute values layout reads — entities decoded,
/// quotes or no quotes — so no spelling of an attribute reaches layout
/// without having passed through here.
fn image_srcs(html: &str) -> Vec<String> {
    let root = parse(html);
    let mut out = Vec::new();
    css_bg_urls(&collect_style_text(&root), &mut out);
    // Iterative: mail nests absurdly deep, and this must not be the thing
    // that overflows the stack. `root` stays alive until the walk is done:
    // rcdom's `Drop` empties the children of every descendant, referenced or
    // not, the moment the last handle to the document goes.
    let mut stack = vec![root.clone()];
    while let Some(node) = stack.pop() {
        let t = tag(&node);
        // Layout never descends into these, so it never asks for their images.
        if is_dropped(t) {
            continue;
        }
        if t == "img" {
            if let Some(src) = attr(&node, "src") {
                out.push(src.trim().to_string());
            }
        }
        if let Some(style) = attr(&node, "style") {
            css_bg_urls(&style, &mut out);
        }
        stack.extend(children(&node).into_iter().rev());
    }
    out
}

/// The `url(...)` arguments of background declarations in a CSS text, as
/// [`crate::style::parse_bg_image`] would read them.
fn css_bg_urls(css: &str, out: &mut Vec<String>) {
    let lower = css.to_ascii_lowercase();
    let bytes = lower.as_bytes();
    let mut from = 0;
    while let Some(p) = lower[from..].find("url(") {
        let at = from + p;
        from = at + 4;
        if at > 0 && (bytes[at - 1].is_ascii_alphanumeric() || bytes[at - 1] == b'-') {
            continue;
        }
        let decl_start = lower[..at].rfind([';', '{', '}']).map_or(0, |i| i + 1);
        if !lower[decl_start..at].trim_start().starts_with("background") {
            continue;
        }
        let inner = &css[at + 4..];
        let Some(end) = inner.find(')') else { break };
        let url = inner[..end].trim().trim_matches(['"', '\'']).trim();
        if !url.is_empty() {
            out.push(url.to_string());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn planned(html: &str, allow: &dyn Fn(&str) -> bool) -> Vec<String> {
        plan(html, allow).into_iter().map(|(s, _)| s).collect()
    }

    fn deny_all(_: &str) -> bool {
        false
    }

    #[test]
    fn cached_reports_what_still_needs_the_network() {
        let html = r#"<img src="https://cached-probe.invalid/a.gif">"#;
        // Denied: nothing to fetch, so the cache-only view is already complete.
        let (_, complete) = HttpResources::cached(html, &deny_all);
        assert!(complete);
        // Allowed but never fetched: incomplete, and no request was made.
        let (res, complete) = HttpResources::cached(html, &|_| true);
        assert!(!complete);
        assert!(res.fetch("https://cached-probe.invalid/a.gif").is_none());
        // Known-bad counts as resolved, as `prefetch` would not retry it.
        cache()
            .lock()
            .unwrap()
            .entries
            .insert("https://cached-probe.invalid/a.gif".to_string(), None);
        let (_, complete) = HttpResources::cached(html, &|_| true);
        assert!(complete);
    }

    /// Every way a mail can spell a remote image. None of it is allowed, so
    /// none of it may be on the download list.
    const SPELLINGS: &str = r#"<html><head>
        <style>.a { background: url(https://t.example/css.gif) }
               .b { background-image: url("https://t.example/css2.gif"); }</style>
        <link rel=stylesheet href=https://t.example/x.css>
        </head><body>
        <img src=https://t.example/unquoted.gif>
        <img src="https://t.example/p.gif?a=1&amp;b=2">
        <img SRC='https://t.example/single.gif'>
        <img src="&#104;ttps://t.example/entity.gif">
        <img srcset="https://t.example/1x.gif 1x, https://t.example/2x.gif 2x">
        <td background=https://t.example/bg.gif>x</td>
        <div style="background:url(https://t.example/style.gif)">x</div>
        <div style="background-image:url(&quot;https://t.example/q.gif&quot;)">x</div>
        <input type=image src=https://t.example/input.gif>
        <video poster=https://t.example/poster.gif></video>
        <img src="//t.example/protocol-relative.gif">
        </body></html>"#;

    #[test]
    fn nothing_unallowed_is_planned_whatever_the_spelling() {
        assert!(planned(SPELLINGS, &deny_all).is_empty());
        // Allowing a different host changes nothing.
        assert!(planned(SPELLINGS, &|h: &str| h == "cdn.example").is_empty());
    }

    #[test]
    fn the_planned_keys_are_what_layout_will_ask_for() {
        let got = planned(SPELLINGS, &|h: &str| h == "t.example");
        // The two background rules, then the body in source order.
        let want = [
            "https://t.example/css.gif",
            "https://t.example/css2.gif",
            "https://t.example/unquoted.gif",
            "https://t.example/p.gif?a=1&b=2", // entity decoded, as layout sees it
            "https://t.example/single.gif",
            "https://t.example/entity.gif",
            "https://t.example/style.gif",
            "https://t.example/q.gif",
            "//t.example/protocol-relative.gif",
        ];
        assert_eq!(got, want);
    }

    #[test]
    fn host_is_what_the_url_parser_sees() {
        let html = r#"<img src="https://allowed.example@evil.example/a.gif">
                      <img src="https://evil.example\@allowed.example/b.gif">
                      <img src="https://ALLOWED.example:8443/c.gif">"#;
        let got = planned(html, &|h: &str| h == "allowed.example");
        assert_eq!(got, ["https://ALLOWED.example:8443/c.gif"]);
    }

    #[test]
    fn non_http_schemes_are_never_planned() {
        let html = r#"<img src="file:///etc/passwd"><img src="ftp://h.example/a">
                      <img src="cid:part1"><img src="data:image/gif;base64,R0lGOD">"#;
        assert!(planned(html, &|_: &str| true).is_empty());
    }

    #[test]
    fn dropped_subtrees_are_not_planned() {
        let html = r#"<video><img src="https://h.example/in-video.gif"></video>
                      <img src="https://h.example/ok.gif">"#;
        assert_eq!(planned(html, &|_: &str| true), ["https://h.example/ok.gif"]);
    }

    #[test]
    fn get_refuses_a_host_the_gate_rejects_without_connecting() {
        // `.invalid` never resolves; a refusal has to come before any lookup.
        let url = Url::parse("https://tracker.invalid/p.gif").unwrap();
        assert!(get(url, &deny_all).is_none());
    }

    #[test]
    fn get_refuses_private_literals() {
        for u in
            ["http://127.0.0.1/x", "http://[::1]/x", "http://0x7f.1/x", "http://169.254.169.254/"]
        {
            let url = Url::parse(u).unwrap();
            assert!(vet(&url).is_none(), "{u}");
        }
    }

    #[test]
    fn v4_classifier() {
        for a in [
            "0.0.0.0",
            "0.1.2.3",
            "10.0.0.1",
            "100.64.0.1",
            "100.127.255.255",
            "127.0.0.1",
            "127.255.255.254",
            "169.254.169.254",
            "172.16.0.1",
            "172.31.255.255",
            "192.0.0.8",
            "192.0.2.1",
            "192.88.99.1",
            "192.168.1.1",
            "198.18.0.1",
            "198.51.100.1",
            "203.0.113.1",
            "224.0.0.1",
            "240.0.0.1",
            "255.255.255.255",
        ] {
            assert!(!is_public_ip(a.parse().unwrap()), "{a} must be refused");
        }
        for a in ["1.1.1.1", "8.8.8.8", "100.63.255.255", "100.128.0.1", "172.15.0.1", "172.32.0.1"]
        {
            assert!(is_public_ip(a.parse().unwrap()), "{a} is public");
        }
    }

    #[test]
    fn v6_classifier() {
        for a in [
            "::",
            "::1",
            "::ffff:127.0.0.1",
            "::ffff:10.1.2.3",
            "::ffff:169.254.169.254",
            "::127.0.0.1",
            "64:ff9b::7f00:1",
            "64:ff9b:1::1",
            "2002:7f00:1::1",
            "2002:c0a8:101::1",
            "2001::1",
            "2001:db8::1",
            "2001:10::1",
            "2001:20::1",
            "fc00::1",
            "fd12:3456::1",
            "fe80::1",
            "fec0::1",
            "ff02::1",
            "100::1",
        ] {
            assert!(!is_public_ip(a.parse().unwrap()), "{a} must be refused");
        }
        for a in [
            "2606:4700:4700::1111",
            "2a00:1450:4001::200e",
            "::ffff:8.8.8.8",
            "64:ff9b::808:808",
            "2002:808:808::1",
        ] {
            assert!(is_public_ip(a.parse().unwrap()), "{a} is public");
        }
    }

    #[test]
    fn css_urls_only_from_background_declarations() {
        let mut out = Vec::new();
        css_bg_urls(
            "@import url(https://h/i.css); @font-face{src:url(https://h/f.woff)} \
             .x{color:red;BACKGROUND-IMAGE: URL( 'https://h/b.gif' )}",
            &mut out,
        );
        assert_eq!(out, ["https://h/b.gif"]);
    }
}
