//! Structural checks over synthetic mail templates (`tests/fixtures/*.html`).
//!
//! These are written for the test, not taken from anyone's inbox, and they
//! check invariants rather than pixels: the bitmap is exactly as wide as asked,
//! the text that should be there is in the text layer (and the hidden text is
//! not), links are found, the height is sane. Pixel references would flap —
//! the stations render with different system fonts (Segoe UI on Windows, Noto
//! Sans on Linux). Pixel-level regression is the job of `examples/golden.rs`
//! over the local corpus.

use emlrender::{render, RenderOptions, Rendered};

struct Case {
    file: &'static str,
    /// Phrases that must be in the text layer at every width.
    must: &'static [&'static str],
    /// Phrases that must not be (hidden content, preheaders).
    must_not: &'static [&'static str],
    /// Plausible height range, CSS px, over all widths.
    height: (f32, f32),
    /// hrefs that must be clickable.
    hrefs: &'static [&'static str],
}

const CASES: &[Case] = &[
    Case {
        file: "newsletter_table.html",
        must: &[
            "Acme Weekly Digest",
            "record number of anvils",
            "Rockets",
            "lifetime warranty",
            "Visit the shop",
            "Unsubscribe",
        ],
        must_not: &["Hiddenpreheader"],
        height: (250.0, 2000.0),
        hrefs: &["https://example.com/shop", "https://example.com/unsubscribe"],
    },
    Case {
        file: "block_cells.html",
        must: &["Order confirmed", "Portable hole", "$61.99", "Delivery", "Track parcel"],
        must_not: &[],
        height: (200.0, 2000.0),
        hrefs: &["https://example.com/track/12345"],
    },
    Case {
        file: "mjml_columns.html",
        must: &["Left column talks about gizmos", "Right column lists widgets", "Get started"],
        must_not: &["Previewtextonly"],
        height: (80.0, 1200.0),
        hrefs: &["https://example.com/start"],
    },
    Case {
        file: "responsive_media.html",
        must: &[
            "Daylight paragraph stays visible",
            "Devicewidth paragraph stays visible",
            "Alpha column",
            "Beta column",
            "Hero banner",
        ],
        must_not: &["Paper edition", "Nighttime banner"],
        height: (150.0, 2500.0),
        hrefs: &["https://example.com/a-very-long-link-that-keeps-going/and-going/and-going/without-any-place-to-break-it"],
    },
    Case {
        file: "combinators.html",
        must: &[
            "Quarterly report",
            "Important notice about invoices.",
            "First item",
            "Third item with new label",
            "Two classes on one paragraph.",
            "Open the full report",
        ],
        must_not: &["Childghost", "Attributeghost", "Inlinedisplay", "should never show"],
        height: (150.0, 1500.0),
        hrefs: &["https://example.com/report"],
    },
];

const WIDTHS: &[u32] = &[320, 420, 600, 800];
const SCALES: &[f32] = &[1.0, 2.0];

fn load(file: &str) -> String {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures").join(file);
    std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()))
}

fn rendered(file: &str, width: u32, scale: f32) -> Rendered {
    render(&load(file), &RenderOptions { width, scale, block_remote: true })
}

/// The text layer as a reader would copy it: runs that continue a word are
/// glued on, everything else is separated by one space.
fn text_of(r: &Rendered) -> String {
    let mut s = String::new();
    for run in &r.runs {
        if !run.cont && !s.is_empty() {
            s.push(' ');
        }
        s.push_str(run.text.trim());
    }
    s.split_whitespace().collect::<Vec<_>>().join(" ")
}

#[test]
fn fixtures_keep_their_structure() {
    let mut failures = Vec::new();
    for case in CASES {
        for &width in WIDTHS {
            for &scale in SCALES {
                let at = format!("{} @{width}x{scale}", case.file);
                let r = rendered(case.file, width, scale);
                let want_w = (width as f32 * scale).round() as u32;
                if r.width_px != want_w {
                    failures.push(format!("{at}: bitmap {} px wide, want {want_w}", r.width_px));
                }
                let text = text_of(&r);
                for phrase in case.must {
                    if !text.contains(phrase) {
                        failures.push(format!("{at}: lost {phrase:?}"));
                    }
                }
                for phrase in case.must_not {
                    if text.contains(phrase) {
                        failures.push(format!("{at}: shows hidden {phrase:?}"));
                    }
                }
                let (_, h) = r.css_size();
                if h < case.height.0 || h > case.height.1 {
                    failures.push(format!("{at}: height {h} outside {:?}", case.height));
                }
                for href in case.hrefs {
                    if !r.links.iter().any(|l| l.href == *href) {
                        failures.push(format!("{at}: no link to {href}"));
                    }
                }
                for l in &r.links {
                    if l.x < -0.5 || l.x + l.w > width as f32 + 0.5 {
                        failures.push(format!("{at}: link box {}..{} outside", l.x, l.x + l.w));
                    }
                }
                for run in &r.runs {
                    if run.x < -0.5 || run.x + run.w > width as f32 + 0.5 {
                        failures.push(format!("{at}: run {:?} sticks out", run.text));
                        break;
                    }
                }
            }
        }
    }
    assert!(failures.is_empty(), "\n{}", failures.join("\n"));
}

/// `@media (max-width: 599.98px)` is evaluated against the render width: a
/// bubble gets the mobile branch, a wide render the desktop one.
#[test]
fn media_queries_follow_the_render_width() {
    for (width, shown, hidden) in [
        (420, "Compact greeting", "Widescreen greeting"),
        (800, "Widescreen greeting", "Compact greeting"),
    ] {
        let text = text_of(&rendered("responsive_media.html", width, 1.0));
        assert!(text.contains(shown), "@{width}: {shown:?} missing in {text:?}");
        assert!(!text.contains(hidden), "@{width}: {hidden:?} shown");
    }
}

/// In the mobile branch the two `td.half` cells become full-width blocks, so
/// the second starts below the first instead of beside it.
#[test]
fn mobile_branch_stacks_the_columns() {
    let r = rendered("responsive_media.html", 420, 1.0);
    let find = |w: &str| r.runs.iter().find(|run| run.text.trim() == w).map(|run| (run.x, run.y));
    let (alpha, beta) = (find("Alpha").expect("Alpha"), find("Beta").expect("Beta"));
    assert!(beta.1 > alpha.1, "stacked: Beta {beta:?} should be below Alpha {alpha:?}");
    assert!((beta.0 - alpha.0).abs() < 2.0, "stacked: same left edge, {alpha:?} vs {beta:?}");

    let r = rendered("responsive_media.html", 800, 1.0);
    let find = |w: &str| r.runs.iter().find(|run| run.text.trim() == w).map(|run| (run.x, run.y));
    let (alpha, beta) = (find("Alpha").expect("Alpha"), find("Beta").expect("Beta"));
    assert!(beta.0 > alpha.0 + 100.0, "side by side on desktop: {alpha:?} vs {beta:?}");
}

/// MJML's columns go side by side once `@media (min-width:480px)` gives them
/// their 50% width, and stack below that.
#[test]
fn mjml_columns_follow_the_breakpoint() {
    let pos = |width: u32| {
        let r = rendered("mjml_columns.html", width, 1.0);
        let find =
            |w: &str| r.runs.iter().find(|run| run.text.trim() == w).map(|run| (run.x, run.y));
        (find("Left").expect("Left"), find("Right").expect("Right"))
    };
    let (left, right) = pos(600);
    assert!(right.0 > left.0 + 100.0 && (right.1 - left.1).abs() < 2.0, "{left:?} {right:?}");
    let (left, right) = pos(420);
    assert!(right.1 > left.1, "stacked below 480px: {left:?} {right:?}");
}

/// Rules that reach their target only through a combinator apply.
#[test]
fn combinator_rules_reach_their_targets() {
    let r = rendered("combinators.html", 420, 1.0);
    let find = |w: &str| r.runs.iter().find(|run| run.text.trim() == w).expect(w);
    // `h1 ~ .after-title` and `.list li:first-child` change nothing a run
    // records, but `.main .notice p { font-weight: bold }` widens the text:
    // compare the notice with the same words set normally.
    let notice = find("invoices.");
    assert!(notice.w > 0.0);
    let plain = render(
        "<p>Important notice about invoices.</p>",
        &RenderOptions { width: 420, scale: 1.0, block_remote: true },
    );
    let plain_w = plain.runs.iter().find(|r| r.text.trim() == "invoices.").expect("plain").w;
    assert!(notice.w > plain_w + 0.5, "bold via descendant rule: {} vs {plain_w}", notice.w);
}
