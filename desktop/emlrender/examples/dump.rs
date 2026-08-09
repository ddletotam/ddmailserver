//! Render every sample mail to `out/*.png` and print a one-line summary each.
//!
//! This is the feedback loop for the renderer: `cargo run --example dump`, then
//! look at the PNGs. Samples are real user mail — they live outside git and
//! must stay that way.

use std::path::{Path, PathBuf};
use std::time::Instant;

use emlrender::{render, RenderOptions, Rendered};

/// One mail, with or without the network resolver behind it.
fn render_one(html: &str, opts: &RenderOptions, load_images: bool) -> Rendered {
    #[cfg(feature = "net")]
    if load_images {
        let images = emlrender::net::HttpResources::prefetch(html);
        return emlrender::render_with(html, opts, &images);
    }
    let _ = load_images;
    render(html, opts)
}

/// The bubble is ~420 CSS px wide; 2× is what a HiDPI client asks for and it
/// makes rendering flaws visible at a glance.
const WIDTH: u32 = 420;
const SCALE: f32 = 2.0;

fn main() {
    let samples = Path::new(env!("CARGO_MANIFEST_DIR")).join("../render-lab/samples");
    let out = Path::new(env!("CARGO_MANIFEST_DIR")).join("out");
    if let Err(e) = std::fs::create_dir_all(&out) {
        eprintln!("cannot create {}: {e}", out.display());
        return;
    }

    let mut files: Vec<PathBuf> = match std::fs::read_dir(&samples) {
        Ok(rd) => rd
            .filter_map(Result::ok)
            .map(|e| e.path())
            .filter(|p| p.extension().is_some_and(|x| x == "html"))
            .collect(),
        Err(e) => {
            eprintln!("cannot read {}: {e}", samples.display());
            return;
        }
    };
    files.sort();

    // `--images` loads what the mail points at, instead of drawing placeholders.
    // Needs `--features net`; it is how you check the render against what the
    // sender actually sees.
    let mut filter: Vec<String> = std::env::args().skip(1).collect();
    let load_images = filter.iter().any(|a| a == "--images");
    filter.retain(|a| a != "--images");
    if load_images && !cfg!(feature = "net") {
        eprintln!("--images needs `--features net`");
        return;
    }
    if !filter.is_empty() {
        files.retain(|p| {
            let name = p.file_name().unwrap_or_default().to_string_lossy().to_string();
            filter.iter().any(|f| name.contains(f.as_str()))
        });
    }

    let mut total_ms = 0u128;
    let mut widest_overflow = 0i64;
    for path in &files {
        let html = std::fs::read_to_string(path).unwrap_or_default();
        // Name first, flushed: if a sample hangs, the log says which one.
        print!("{:34} ", path.file_stem().unwrap_or_default().to_string_lossy());
        let _ = std::io::Write::flush(&mut std::io::stdout());
        let t0 = Instant::now();
        let opts = RenderOptions { width: WIDTH, scale: SCALE, block_remote: !load_images };
        let r = render_one(&html, &opts, load_images);
        let ms = t0.elapsed().as_millis();
        total_ms += ms;

        let limit = (WIDTH as f32 * SCALE) as i64;
        let overflow = r.width_px as i64 - limit;
        widest_overflow = widest_overflow.max(overflow);

        let name = path.file_stem().unwrap_or_default().to_string_lossy().to_string();
        println!(
            "{:>5}x{:<6} links={:<4} runs={:<5} {ms:>5}ms{}",
            r.width_px,
            r.height_px,
            r.links.len(),
            r.runs.len(),
            if overflow > 0 { "  ** OVERFLOW **" } else { "" },
        );

        // Mails are drawn on the bubble's background, so flatten onto white
        // rather than shipping a transparent PNG that is hard to eyeball.
        let mut flat = Vec::with_capacity(r.rgba.len() / 4 * 3);
        for px in r.rgba.chunks_exact(4) {
            let a = px[3] as u32;
            let over = |c: u8| ((c as u32 * a + 255 * (255 - a)) / 255).min(255) as u8;
            flat.extend_from_slice(&[over(px[0]), over(px[1]), over(px[2])]);
        }
        let img = image::RgbImage::from_raw(r.width_px, r.height_px, flat);
        match img {
            Some(img) => {
                if let Err(e) = img.save(out.join(format!("{name}.png"))) {
                    eprintln!("  save failed: {e}");
                }
            }
            None => eprintln!("  bad bitmap dimensions"),
        }
    }

    println!("\n{} samples, {total_ms} ms total", files.len());
    if widest_overflow > 0 {
        println!("WIDTH INVARIANT BROKEN by {widest_overflow}px");
    } else {
        println!("width invariant holds");
    }
}
