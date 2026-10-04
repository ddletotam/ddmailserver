//! Golden regression over the local sample corpus: render every sample and
//! compare with the stored reference.
//!
//! ```text
//! cargo run --release --example golden                 # check against the reference
//! cargo run --release --example golden -- --bless      # overwrite the reference
//! cargo run --release --example golden -- --wrap       # inside the client's bubble chrome
//! cargo run --release --example golden -- --repeat 5   # best of 5 per mail (timing)
//! cargo run --release --example golden -- 013 022      # only these
//! ```
//!
//! The reference lives next to the samples, in `samples/.golden/<mode>/`: an
//! `index.tsv` (size, links, runs and a hash of the bitmap per mail), plus the
//! bitmap itself as PNG and its text layer as `.txt`, so a change can be looked
//! at and not only counted. Samples are the user's real mail, so the reference
//! — made from them — stays out of git exactly like they do (`samples/` is in
//! `render-lab/.gitignore`).
//!
//! On a mismatch the tool prints the metrics that moved and writes
//! `out/golden-diff/<name>.png`: reference | current | changed pixels in red.
//! Exit status is 1 when anything changed, so it can gate a script.

use std::collections::{BTreeMap, HashMap};
use std::path::{Path, PathBuf};
use std::time::Instant;

use emlrender::{render, RenderOptions, Rendered};

/// The client's bubble chrome, copied from `bubble_template_wide` in
/// `desktop/native/src/main.rs` (incoming, wide). Keep in sync by hand: the
/// point of `--wrap` is to see what the client's own stylesheet does to a mail
/// once the renderer starts honouring more of it.
const WRAP_HEAD: &str = r#"<!DOCTYPE html><html><head><meta charset="utf-8"><style>
        html, body { margin: 0; padding: 0; background: #e9eef5; }
        body { font-family: 'Segoe UI', system-ui, sans-serif; }
        .ddm-row { padding: 6px 60px; }
        .ddm-bubble-out { margin-left: auto; margin-right: 0; }
        .ddm-bubble-in  { margin-left: 0; margin-right: auto; }
        .ddm-bubble {
            max-width: 72%; background: #ffffff; border-radius: 16px; padding: 10px 14px;
            font-size: 15px; line-height: 1.4; color: #0f1419;
            box-shadow: 0 1px 2px rgba(0,0,0,0.12); overflow-wrap: anywhere;
        }
        .ddm-wide { max-width: 100%; }
        .ddm-bubble-out { border-bottom-right-radius: 4px; }
        .ddm-bubble-in  { border-bottom-left-radius: 4px; }
        .ddm-bubble * { max-width: 100% !important; border: 0 !important; background-image: none !important; }
        .ddm-bubble table, .ddm-bubble td, .ddm-bubble th { border-collapse: collapse !important; }
        .ddm-bubble img { max-width: 100% !important; height: auto !important; }
        a { color: #10b981; }
        .ddm-time { text-align: right; font-size: 11px; color: #8a97a5;
                 margin-top: 4px; user-select: none; }
        </style></head>
        <body><div class="ddm-row"><div class="ddm-bubble ddm-bubble-in ddm-wide">"#;
const WRAP_TAIL: &str = r#"<div class="ddm-time">12:00</div></div></div></body></html>"#;

struct Args {
    bless: bool,
    wrap: bool,
    width: u32,
    scale: f32,
    repeat: u32,
    filter: Vec<String>,
}

fn parse_args() -> Args {
    let mut a =
        Args { bless: false, wrap: false, width: 420, scale: 2.0, repeat: 1, filter: vec![] };
    let mut it = std::env::args().skip(1);
    while let Some(arg) = it.next() {
        match arg.as_str() {
            "--bless" => a.bless = true,
            "--wrap" => a.wrap = true,
            "--width" => a.width = it.next().and_then(|v| v.parse().ok()).unwrap_or(a.width),
            "--scale" => a.scale = it.next().and_then(|v| v.parse().ok()).unwrap_or(a.scale),
            "--repeat" => a.repeat = it.next().and_then(|v| v.parse().ok()).unwrap_or(1).max(1),
            _ => a.filter.push(arg),
        }
    }
    a
}

/// One reference entry.
#[derive(Clone, PartialEq)]
struct Entry {
    w: u32,
    h: u32,
    links: usize,
    runs: usize,
    hash: u64,
}

fn fnv1a(bytes: &[u8]) -> u64 {
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    for b in bytes {
        h ^= *b as u64;
        h = h.wrapping_mul(0x0100_0000_01b3);
    }
    h
}

fn entry_of(r: &Rendered) -> Entry {
    let mut h = fnv1a(&r.rgba);
    h ^= (r.width_px as u64) << 32 | r.height_px as u64;
    Entry { w: r.width_px, h: r.height_px, links: r.links.len(), runs: r.runs.len(), hash: h }
}

fn read_index(path: &Path) -> BTreeMap<String, Entry> {
    let mut out = BTreeMap::new();
    let Ok(text) = std::fs::read_to_string(path) else { return out };
    for line in text.lines() {
        let f: Vec<&str> = line.split('\t').collect();
        if f.len() != 6 {
            continue;
        }
        let num = |s: &str| s.parse::<u64>().ok();
        let (Some(w), Some(h), Some(l), Some(r)) = (num(f[1]), num(f[2]), num(f[3]), num(f[4]))
        else {
            continue;
        };
        let Ok(hash) = u64::from_str_radix(f[5], 16) else { continue };
        let e = Entry { w: w as u32, h: h as u32, links: l as usize, runs: r as usize, hash };
        out.insert(f[0].to_string(), e);
    }
    out
}

fn write_index(path: &Path, index: &BTreeMap<String, Entry>) -> std::io::Result<()> {
    let mut s = String::new();
    for (name, e) in index {
        s.push_str(&format!(
            "{name}\t{}\t{}\t{}\t{}\t{:016x}\n",
            e.w, e.h, e.links, e.runs, e.hash
        ));
    }
    std::fs::write(path, s)
}

fn text_layer(r: &Rendered) -> String {
    let mut s = String::new();
    for run in &r.runs {
        s.push_str(&run.text);
        s.push('\n');
    }
    s
}

/// Words that appear more often on one side than on the other.
fn word_delta(old: &str, new: &str) -> (Vec<String>, Vec<String>) {
    let count = |t: &str| {
        let mut m: HashMap<String, i64> = HashMap::new();
        for w in t.split_whitespace() {
            *m.entry(w.to_string()).or_default() += 1;
        }
        m
    };
    let (a, b) = (count(old), count(new));
    let mut lost = Vec::new();
    let mut gained = Vec::new();
    for (w, n) in &a {
        for _ in 0..(n - b.get(w).copied().unwrap_or(0)).max(0) {
            lost.push(w.clone());
        }
    }
    for (w, n) in &b {
        for _ in 0..(n - a.get(w).copied().unwrap_or(0)).max(0) {
            gained.push(w.clone());
        }
    }
    lost.sort();
    gained.sort();
    (lost, gained)
}

fn save_rgba(path: &Path, w: u32, h: u32, rgba: &[u8]) {
    match image::RgbaImage::from_raw(w, h, rgba.to_vec()) {
        Some(img) => {
            if let Err(e) = img.save(path) {
                eprintln!("  save {} failed: {e}", path.display());
            }
        }
        None => eprintln!("  bad bitmap dimensions for {}", path.display()),
    }
}

/// Pixel over white, as the eye sees it in the bubble.
fn flat(px: &[u8]) -> [u8; 3] {
    let a = px[3] as u32;
    let over = |c: u8| ((c as u32 * a + 255 * (255 - a)) / 255).min(255) as u8;
    [over(px[0]), over(px[1]), over(px[2])]
}

/// Reference | current | difference (changed pixels red over a faded current).
/// Returns the share of changed pixels in the common area.
fn write_diff(path: &Path, old: &image::RgbaImage, new: &Rendered) -> f32 {
    let (ow, oh) = old.dimensions();
    let (nw, nh) = (new.width_px, new.height_px);
    let gap = 8u32;
    let h = oh.max(nh);
    let cw = ow.max(nw);
    let total_w = ow + gap + nw + gap + cw;
    let mut out = image::RgbImage::from_pixel(total_w, h, image::Rgb([128, 128, 128]));
    let new_px = |x: u32, y: u32| -> Option<&[u8]> {
        (x < nw && y < nh).then(|| {
            let i = ((y * nw + x) * 4) as usize;
            &new.rgba[i..i + 4]
        })
    };
    let mut changed = 0u64;
    for y in 0..h {
        for x in 0..cw {
            let o = (x < ow && y < oh).then(|| old.get_pixel(x, y).0);
            let n = new_px(x, y);
            if let Some(o) = o {
                out.put_pixel(x, y, image::Rgb(flat(&o)));
            }
            if let Some(n) = n {
                out.put_pixel(ow + gap + x, y, image::Rgb(flat(n)));
            }
            let px = match (o, n) {
                (Some(o), Some(n)) if o[..] == n[..] => {
                    let f = flat(n);
                    let fade = |c: u8| (c as u32 * 3 / 10 + 255 * 7 / 10) as u8;
                    [fade(f[0]), fade(f[1]), fade(f[2])]
                }
                (None, None) => continue,
                _ => {
                    changed += 1;
                    [230, 0, 0]
                }
            };
            out.put_pixel(ow + gap + nw + gap + x, y, image::Rgb(px));
        }
    }
    if let Err(e) = out.save(path) {
        eprintln!("  save {} failed: {e}", path.display());
    }
    changed as f32 / (cw as f32 * h as f32).max(1.0)
}

fn main() {
    let args = parse_args();
    let root = Path::new(env!("CARGO_MANIFEST_DIR"));
    let samples = std::env::var_os("EMLRENDER_SAMPLES")
        .map(PathBuf::from)
        .unwrap_or_else(|| root.join("../render-lab/samples"));
    let mode = format!("{}w{}x{}", if args.wrap { "wrap-" } else { "" }, args.width, args.scale);
    let golden = samples.join(".golden").join(&mode);
    let diff_dir = root.join("out/golden-diff").join(&mode);
    for d in [&golden, &diff_dir] {
        if let Err(e) = std::fs::create_dir_all(d) {
            eprintln!("cannot create {}: {e}", d.display());
            std::process::exit(2);
        }
    }

    let mut files: Vec<PathBuf> = match std::fs::read_dir(&samples) {
        Ok(rd) => rd
            .filter_map(Result::ok)
            .map(|e| e.path())
            .filter(|p| p.extension().is_some_and(|x| x == "html"))
            .collect(),
        Err(e) => {
            eprintln!("cannot read {}: {e}", samples.display());
            std::process::exit(2);
        }
    };
    files.sort();
    if !args.filter.is_empty() {
        files.retain(|p| {
            let name = p.file_name().unwrap_or_default().to_string_lossy().to_string();
            args.filter.iter().any(|f| name.contains(f.as_str()))
        });
    }

    let index_path = golden.join("index.tsv");
    let mut index = read_index(&index_path);
    let opts = RenderOptions { width: args.width, scale: args.scale, block_remote: true };
    let limit = (args.width as f32 * args.scale).round() as u32;

    let mut total_ms = 0f64;
    let mut changed = 0usize;
    let mut missing = 0usize;
    let mut overflow = 0usize;
    for path in &files {
        let name = path.file_stem().unwrap_or_default().to_string_lossy().to_string();
        let raw = std::fs::read_to_string(path).unwrap_or_default();
        let html = if args.wrap { format!("{WRAP_HEAD}{raw}{WRAP_TAIL}") } else { raw };

        // Best of N: the first render of a run also pays for the font scan,
        // and a single pass on a desktop is noisy.
        let mut best = f64::MAX;
        let mut r = None;
        for _ in 0..args.repeat {
            let t0 = Instant::now();
            let out = render(&html, &opts);
            best = best.min(t0.elapsed().as_secs_f64() * 1000.0);
            r = Some(out);
        }
        let Some(r) = r else { continue };
        total_ms += best;
        if r.width_px > limit {
            overflow += 1;
            println!("{name:34} ** OVERFLOW ** {} > {limit}", r.width_px);
        }

        let now = entry_of(&r);
        if args.bless {
            save_rgba(&golden.join(format!("{name}.png")), r.width_px, r.height_px, &r.rgba);
            if let Err(e) = std::fs::write(golden.join(format!("{name}.txt")), text_layer(&r)) {
                eprintln!("  save text failed: {e}");
            }
            index.insert(name, now);
            continue;
        }

        let Some(old) = index.get(&name) else {
            missing += 1;
            println!("{name:34} (no reference)");
            continue;
        };
        if *old == now {
            continue;
        }
        changed += 1;
        let mut line = format!("{name:34}");
        if (old.w, old.h) != (now.w, now.h) {
            line.push_str(&format!(" size {}x{}→{}x{}", old.w, old.h, now.w, now.h));
        }
        if old.links != now.links {
            line.push_str(&format!(" links {}→{}", old.links, now.links));
        }
        if old.runs != now.runs {
            line.push_str(&format!(" runs {}→{}", old.runs, now.runs));
        }
        if let Ok(old_img) = image::open(golden.join(format!("{name}.png"))) {
            let share = write_diff(&diff_dir.join(format!("{name}.png")), &old_img.to_rgba8(), &r);
            line.push_str(&format!(" pixels {:.1}%", share * 100.0));
        }
        if let Ok(old_text) = std::fs::read_to_string(golden.join(format!("{name}.txt"))) {
            let (lost, gained) = word_delta(&old_text, &text_layer(&r));
            if !lost.is_empty() || !gained.is_empty() {
                line.push_str(&format!(" words -{} +{}", lost.len(), gained.len()));
            }
            if !lost.is_empty() {
                let shown: Vec<&str> = lost.iter().take(8).map(String::as_str).collect();
                line.push_str(&format!("\n{:34}   lost: {}", "", shown.join(" ")));
            }
        }
        println!("{line}");
    }

    if args.bless {
        if let Err(e) = write_index(&index_path, &index) {
            eprintln!("cannot write {}: {e}", index_path.display());
            std::process::exit(2);
        }
        println!("blessed {} samples into {}", files.len(), golden.display());
    } else {
        println!(
            "\n{} samples, {changed} changed, {missing} without reference; diffs in {}",
            files.len(),
            diff_dir.display()
        );
    }
    println!("render total {total_ms:.0} ms (best of {} per mail)", args.repeat);
    if overflow > 0 {
        println!("WIDTH INVARIANT BROKEN in {overflow} samples");
    }
    if overflow > 0 || changed > 0 {
        std::process::exit(1);
    }
}
