//! Disk-persistent cache of rendered bubble textures.
//!
//! The render worker's RAM cache dies with the process, so every restart
//! re-rendered every bubble through the WebView — seconds per conversation
//! that this layer turns into a PNG decode (~ms). One entry = a PNG of the
//! RGBA bitmap + a JSON sidecar `{h, links}`; the key mirrors the RAM key
//! `(folder, uid, width, policy_gen)`. policy_gen persists across restarts
//! (see policy::Policy::gen), so stale-policy textures can never resurrect.
//!
//! Filenames are an FNV-1a hash of the key: deterministic across processes
//! (std's RandomState is seeded per-process and would orphan every file).

use std::fs;
use std::path::PathBuf;
use std::time::SystemTime;

use crate::render_common::{parse_link_rects, parse_text_runs, LinkRect, TextRun};

/// Bump when the sidecar format gains fields (part of the filename key):
/// v2 = + text runs. v3 = bubble corner timestamp added to the rendered HTML.
/// v4 = timestamp unit fix (date_ts is seconds, was mis-rendered as 1970).
/// v5 = geometry rework: rects extracted post-viewport-grow (no scrollbar
/// reflow), per-line-fragment link/word rects, DPI-scaled bitmaps with `h`
/// now logical (CSS px) rather than bitmap px.
/// Old entries simply miss and re-render once.
const FORMAT_VERSION: u32 = 5;

/// Disk budget. Eviction drops oldest-by-mtime entries until under cap;
/// runs once at startup (steady-state growth between starts is modest).
const MAX_DISK_BYTES: u64 = 300 * 1024 * 1024;

/// Deterministic FNV-1a (process-stable, unlike std's RandomState).
/// Public: the render worker also fingerprints body HTML with it.
pub fn fnv1a(s: &str) -> u64 {
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    for b in s.as_bytes() {
        h ^= *b as u64;
        h = h.wrapping_mul(0x0000_0100_0000_01b3);
    }
    h
}

pub struct TextureDiskCache {
    dir: PathBuf,
}

/// A loaded entry: raw RGBA + pixel dims + logical height + link rects +
/// the per-word text layer for selection.
pub struct DiskEntry {
    pub rgba: Vec<u8>,
    pub width: u32,
    pub height: u32,
    pub h: f32,
    pub links: Vec<LinkRect>,
    pub runs: Vec<TextRun>,
}

impl TextureDiskCache {
    /// Open (and create) the cache directory next to cache.db. None when
    /// no per-user data dir can be resolved — caller just runs RAM-only.
    pub fn open(data_dir: PathBuf) -> Option<Self> {
        let dir = data_dir.join("textures");
        fs::create_dir_all(&dir).ok()?;
        let cache = Self { dir };
        cache.evict_to_cap();
        Some(cache)
    }

    fn base(&self, folder: &str, uid: u32, width: u32, policy_gen: u64, mode: u8, fp: u64) -> PathBuf {
        let key = format!("{folder}|{uid}|{width}|{policy_gen}|{mode}|{fp}|v{FORMAT_VERSION}");
        self.dir.join(format!("{:016x}", fnv1a(&key)))
    }

    pub fn load(&self, folder: &str, uid: u32, width: u32, policy_gen: u64, mode: u8, fp: u64) -> Option<DiskEntry> {
        let base = self.base(folder, uid, width, policy_gen, mode, fp);
        let meta_raw = fs::read_to_string(base.with_extension("json")).ok()?;
        let meta: serde_json::Value = serde_json::from_str(&meta_raw).ok()?;
        let h = meta.get("h")?.as_f64()? as f32;
        let links = meta
            .get("links")
            .map(|l| parse_link_rects(&l.to_string()))
            .unwrap_or_default();
        let runs = meta
            .get("runs")
            .map(|r| parse_text_runs(&r.to_string()))
            .unwrap_or_default();
        let img = image::open(base.with_extension("png")).ok()?.into_rgba8();
        let (width_px, height_px) = img.dimensions();
        Some(DiskEntry {
            rgba: img.into_raw(),
            width: width_px,
            height: height_px,
            h,
            links,
            runs,
        })
    }

    /// Best-effort store; failures only cost a future re-render.
    pub fn store(
        &self,
        folder: &str,
        uid: u32,
        width: u32,
        policy_gen: u64,
        mode: u8,
        fp: u64,
        rgba: &[u8],
        width_px: u32,
        height_px: u32,
        h: f32,
        links: &[LinkRect],
        runs: &[TextRun],
    ) {
        if rgba.is_empty() || width_px == 0 || height_px == 0 {
            return;
        }
        // image::save_buffer asserts on a length mismatch and would panic the
        // render worker (which then can't render any later conversation). A
        // malformed bitmap is skipped, not fatal — it just re-renders later.
        let expected = (width_px as usize) * (height_px as usize) * 4;
        if rgba.len() != expected {
            eprintln!(
                "texture store: bad buffer {}x{} — {} bytes, expected {} — skipping",
                width_px, height_px, rgba.len(), expected
            );
            return;
        }
        let base = self.base(folder, uid, width, policy_gen, mode, fp);
        let links_json: Vec<serde_json::Value> = links
            .iter()
            .map(|l| {
                serde_json::json!({
                    "x": l.x, "y": l.y, "w": l.w, "h": l.h, "href": l.href,
                })
            })
            .collect();
        let runs_json: Vec<serde_json::Value> = runs
            .iter()
            .map(|r| {
                serde_json::json!({
                    "x": r.x, "y": r.y, "w": r.w, "h": r.h, "t": r.text,
                    "c": i32::from(r.cont),
                })
            })
            .collect();
        let meta = serde_json::json!({ "h": h, "links": links_json, "runs": runs_json });
        if image::save_buffer(
            base.with_extension("png"),
            rgba,
            width_px,
            height_px,
            image::ExtendedColorType::Rgba8,
        )
        .is_err()
        {
            return;
        }
        let _ = fs::write(base.with_extension("json"), meta.to_string());
    }

    /// Delete oldest entries (by PNG mtime) until total size is under cap.
    fn evict_to_cap(&self) {
        let Ok(entries) = fs::read_dir(&self.dir) else { return };
        let mut pngs: Vec<(PathBuf, u64, SystemTime)> = Vec::new();
        let mut total: u64 = 0;
        for e in entries.flatten() {
            let path = e.path();
            let Ok(md) = e.metadata() else { continue };
            total += md.len();
            if path.extension().map(|x| x == "png").unwrap_or(false) {
                let mtime = md.modified().unwrap_or(SystemTime::UNIX_EPOCH);
                pngs.push((path, md.len(), mtime));
            }
        }
        if total <= MAX_DISK_BYTES {
            return;
        }
        pngs.sort_by_key(|(_, _, mtime)| *mtime);
        for (path, len, _) in pngs {
            if total <= MAX_DISK_BYTES {
                break;
            }
            let _ = fs::remove_file(path.with_extension("json"));
            if fs::remove_file(&path).is_ok() {
                total = total.saturating_sub(len);
            }
        }
    }
}
