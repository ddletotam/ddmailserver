//! An HTTP [`Resources`](crate::Resources) implementation — **only** when the
//! `net` feature is on.
//!
//! The renderer proper never opens a socket, and that stays true: this module
//! is not reachable from `render()`, it is a resolver a host may choose to pass
//! to `render_with()`. It lives here rather than in each host because both the
//! client and the harness want the same thing — a bounded, cached, parallel
//! prefetch — and two copies of that drift.
//!
//! Deciding *whether* an image may be loaded is still the host's business.
//! Loading one tells its sender the message was opened, which is why the client
//! gates it per sender before the HTML ever reaches a renderer.

use std::collections::HashMap;
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use crate::Resources;

/// Per-message ceilings. A mail past any of them renders with placeholders for
/// the remainder rather than holding the panel hostage.
const MAX_IMAGES: usize = 64;
const MAX_BYTES: usize = 8 * 1024 * 1024;
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

fn client() -> &'static reqwest::blocking::Client {
    static CLIENT: OnceLock<reqwest::blocking::Client> = OnceLock::new();
    CLIENT.get_or_init(|| {
        reqwest::blocking::Client::builder()
            .timeout(REQUEST_TIMEOUT)
            .redirect(reqwest::redirect::Policy::limited(4))
            // Some CDNs answer an empty agent with a 403.
            .user_agent("Mozilla/5.0 (compatible; ddmail)")
            .build()
            .unwrap_or_default()
    })
}

/// Everything fetched for one message, keyed by the `src` as it was written.
pub struct HttpResources(HashMap<String, Arc<Vec<u8>>>);

impl Resources for HttpResources {
    fn fetch(&self, src: &str) -> Option<Vec<u8>> {
        self.0.get(src.trim()).map(|b| b.as_ref().clone())
    }
}

impl HttpResources {
    /// Fetch every remote `<img src>` in `html`, in parallel, then hand the
    /// result to `render_with`.
    ///
    /// A *pre*-fetch rather than a lazy callback for two reasons: layout asks
    /// for images one at a time on one thread, so a newsletter with thirty of
    /// them would serialise thirty round trips into a visibly stalled bubble;
    /// and a deadline for the batch is only enforceable if something owns the
    /// batch.
    pub fn prefetch(html: &str) -> Self {
        let mut wanted: Vec<String> = Vec::new();
        for src in image_srcs(html) {
            if wanted.len() >= MAX_IMAGES {
                break;
            }
            if !wanted.contains(&src) {
                wanted.push(src);
            }
        }

        let mut out: HashMap<String, Arc<Vec<u8>>> = HashMap::new();
        let mut todo: Vec<String> = Vec::new();
        {
            let cache = cache().lock().unwrap_or_else(|p| p.into_inner());
            for src in wanted {
                match cache.entries.get(&src) {
                    Some(Some(bytes)) => {
                        out.insert(src, Arc::clone(bytes));
                    }
                    Some(None) => {} // known-bad; asking again costs a timeout
                    None => todo.push(src),
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
                    let Some(src) = next else { return };
                    let bytes = get(&src);
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

fn get(src: &str) -> Option<Vec<u8>> {
    let url = absolute(src)?;
    let resp = client().get(&url).send().ok()?;
    if !resp.status().is_success() {
        return None;
    }
    // Trust the header when it is there and the body when it is not — a
    // chunked response can lie by omission.
    if resp.content_length().is_some_and(|n| n as usize > MAX_BYTES) {
        return None;
    }
    let body = resp.bytes().ok()?;
    if body.is_empty() || body.len() > MAX_BYTES {
        return None;
    }
    Some(body.to_vec())
}

/// `//cdn.example/x.png` is a real thing in mail; everything else must already
/// carry a scheme we are willing to speak.
fn absolute(src: &str) -> Option<String> {
    let src = src.trim();
    if starts_ci(src, "http://") || starts_ci(src, "https://") {
        Some(src.to_string())
    } else if src.starts_with("//") {
        Some(format!("https:{src}"))
    } else {
        None
    }
}

fn starts_ci(s: &str, prefix: &str) -> bool {
    s.len() >= prefix.len() && s[..prefix.len()].eq_ignore_ascii_case(prefix)
}

/// Every remote `<img src>` in the document, in source order.
///
/// A hand-rolled scan rather than a parse or a regex: this runs before layout,
/// so parsing here would parse the document twice, and the crate has no reason
/// to carry a regex engine for thirty lines of work.
fn image_srcs(html: &str) -> Vec<String> {
    let bytes = html.as_bytes();
    let mut out = Vec::new();
    let mut i = 0;
    while let Some(at) = find_ci(bytes, i, b"<img") {
        i = at + 4;
        // The tag must actually end somewhere; unterminated means truncated.
        let Some(end) = bytes[i..].iter().position(|b| *b == b'>').map(|p| p + i) else {
            break;
        };
        if let Some(src) = tag_src(&html[i..end]) {
            if absolute(src).is_some() {
                out.push(src.trim().to_string());
            }
        }
        i = end + 1;
    }
    out
}

/// The value of the `src` attribute inside one tag's attribute text.
fn tag_src(attrs: &str) -> Option<&str> {
    let bytes = attrs.as_bytes();
    let mut i = 0;
    while let Some(at) = find_ci(bytes, i, b"src") {
        i = at + 3;
        // `data-blocked-src` and friends must not match: the character before
        // `src` has to be a boundary, and what follows has to be `=`.
        let boundary = at == 0 || !is_name_char(bytes[at - 1]);
        let mut j = i;
        while j < bytes.len() && bytes[j].is_ascii_whitespace() {
            j += 1;
        }
        if !boundary || j >= bytes.len() || bytes[j] != b'=' {
            continue;
        }
        j += 1;
        while j < bytes.len() && bytes[j].is_ascii_whitespace() {
            j += 1;
        }
        if j >= bytes.len() {
            return None;
        }
        let quote = bytes[j];
        return if quote == b'"' || quote == b'\'' {
            let start = j + 1;
            bytes[start..].iter().position(|b| *b == quote).map(|p| &attrs[start..start + p])
        } else {
            let end = bytes[j..]
                .iter()
                .position(|b| b.is_ascii_whitespace())
                .map_or(attrs.len(), |p| j + p);
            Some(&attrs[j..end])
        };
    }
    None
}

fn is_name_char(b: u8) -> bool {
    b.is_ascii_alphanumeric() || b == b'-' || b == b'_' || b == b':'
}

fn find_ci(haystack: &[u8], from: usize, needle: &[u8]) -> Option<usize> {
    if from >= haystack.len() || needle.is_empty() {
        return None;
    }
    haystack[from..]
        .windows(needle.len())
        .position(|w| w.eq_ignore_ascii_case(needle))
        .map(|p| p + from)
}
