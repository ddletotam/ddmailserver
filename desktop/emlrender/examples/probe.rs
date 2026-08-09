//! Render one file, optionally only its first N bytes, and report the time.
//!
//! Bisecting a hang: `cargo run --example probe -- <file> <prefix-bytes>`.
//! Truncated HTML is fine — html5ever closes whatever is left open.

use std::time::Instant;

use emlrender::{RenderOptions, Rendered};

/// With `--features net` the probe loads what the mail points at, so what you
/// look at is what the sender sees.
fn render_one(html: &str, opts: &RenderOptions) -> Rendered {
    #[cfg(feature = "net")]
    {
        let images = emlrender::net::HttpResources::prefetch(html);
        emlrender::render_with(html, opts, &images)
    }
    #[cfg(not(feature = "net"))]
    emlrender::render(html, opts)
}

fn main() {
    let mut args = std::env::args().skip(1);
    let Some(path) = args.next() else {
        eprintln!("usage: probe <file.html> [prefix-bytes]");
        return;
    };
    let prefix: usize = args.next().and_then(|v| v.parse().ok()).unwrap_or(usize::MAX);

    let html = std::fs::read_to_string(&path).unwrap_or_default();
    let cut = html
        .char_indices()
        .map(|(i, _)| i)
        .chain(std::iter::once(html.len()))
        .take_while(|i| *i <= prefix)
        .last()
        .unwrap_or(0);
    let html = &html[..cut];
    println!("{} bytes of {path}", html.len());

    let t0 = Instant::now();
    // `PROBE_W` / `PROBE_SCALE` to match a caller other than the bubble —
    // the source viewer renders at 760 px, for instance.
    let width = std::env::var("PROBE_W").ok().and_then(|v| v.parse().ok()).unwrap_or(420);
    let scale = std::env::var("PROBE_SCALE").ok().and_then(|v| v.parse().ok()).unwrap_or(2.0);
    let opts = RenderOptions { width, scale, block_remote: !cfg!(feature = "net") };
    let r = render_one(html, &opts);
    println!(
        "{}x{} links={} runs={} in {} ms",
        r.width_px,
        r.height_px,
        r.links.len(),
        r.runs.len(),
        t0.elapsed().as_millis()
    );

    if std::env::var_os("EMLRENDER_DEBUG").is_some() {
        for run in &r.runs {
            println!("  run {:>7.1},{:<7.1} {:>6.1}x{:<5.1} {:?}", run.x, run.y, run.w, run.h, run.text);
        }
    }

    let mut flat = Vec::with_capacity(r.rgba.len() / 4 * 3);
    for px in r.rgba.chunks_exact(4) {
        let a = px[3] as u32;
        let over = |c: u8| ((c as u32 * a + 255 * (255 - a)) / 255).min(255) as u8;
        flat.extend_from_slice(&[over(px[0]), over(px[1]), over(px[2])]);
    }
    let out = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("out/probe.png");
    let _ = std::fs::create_dir_all(out.parent().unwrap_or(std::path::Path::new(".")));
    match image::RgbImage::from_raw(r.width_px, r.height_px, flat) {
        Some(img) => match img.save(&out) {
            Ok(()) => println!("{}", out.display()),
            Err(e) => eprintln!("save failed: {e}"),
        },
        None => eprintln!("bad bitmap dimensions"),
    }
}
