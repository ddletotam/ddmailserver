//! `@media` query evaluation against the render viewport.
//!
//! A bubble is 420–600 CSS px wide, which is a phone, not a desktop: the
//! `@media (max-width: 600px)` branch of a template is the one written for
//! exactly this width, and skipping it lays a 600 px desktop design into a
//! narrow column.
//!
//! The viewport is the render width in CSS px. Supported: media types
//! `all` / `screen` (`print` and the rest are false), `only`, `not`, `and`,
//! comma lists, `width` / `min-width` / `max-width` in px, em, rem or pt,
//! Level 4 ranges (`(width <= 600px)`, `(400px < width < 700px)`) and
//! `prefers-color-scheme` (we paint light). Every other feature — device-width,
//! resolution, orientation, hover — evaluates to false: guessing true for a
//! feature we cannot know is how a print stylesheet ends up on screen.

/// Does `query` (the prelude of `@media`, or a `media=""` attribute) hold for a
/// viewport `width` CSS px wide?
pub fn matches(query: &str, width: f32) -> bool {
    let q = query.trim();
    if q.is_empty() {
        return true;
    }
    split_top_level(q).iter().any(|one| one_query(one, width))
}

fn split_top_level(q: &str) -> Vec<&str> {
    let mut out = Vec::new();
    let mut depth = 0i32;
    let mut start = 0usize;
    for (i, c) in q.char_indices() {
        match c {
            '(' => depth += 1,
            ')' => depth -= 1,
            ',' if depth == 0 => {
                out.push(&q[start..i]);
                start = i + 1;
            }
            _ => {}
        }
    }
    out.push(&q[start..]);
    out
}

fn one_query(q: &str, width: f32) -> bool {
    let lower = q.trim().to_ascii_lowercase();
    // Words outside parentheses, and the parenthesised features.
    let mut words = Vec::new();
    let mut features = Vec::new();
    let mut rest = lower.as_str();
    loop {
        let open = rest.find('(');
        let head = &rest[..open.unwrap_or(rest.len())];
        words.extend(head.split_whitespace().map(str::to_string));
        let Some(open) = open else { break };
        let Some(close) = rest[open..].find(')') else { return false };
        features.push(rest[open + 1..open + close].trim().to_string());
        rest = &rest[open + close + 1..];
    }

    let mut negate = false;
    let mut words = words.as_slice();
    match words.first().map(String::as_str) {
        Some("not") => {
            negate = true;
            words = &words[1..];
        }
        Some("only") => words = &words[1..],
        _ => {}
    }
    let mut ok = true;
    let mut expect_type = true;
    for w in words {
        match w.as_str() {
            "and" => expect_type = false,
            "all" | "screen" if expect_type => expect_type = false,
            // `print`, `speech`, `tv`, … and anything we cannot read.
            _ => ok = false,
        }
    }
    if ok {
        ok = features.iter().all(|f| feature(f, width));
    }
    ok != negate
}

fn feature(f: &str, width: f32) -> bool {
    if let Some((name, value)) = f.split_once(':') {
        let name = name.trim();
        let value = value.trim();
        return match name {
            "width" => length(value).is_some_and(|v| (width - v).abs() < 0.01),
            "min-width" => length(value).is_some_and(|v| width >= v),
            "max-width" => length(value).is_some_and(|v| width <= v),
            "prefers-color-scheme" => value == "light",
            _ => false,
        };
    }
    range(f, width).unwrap_or(false)
}

/// `width <= 600px`, `600px >= width`, `400px < width <= 700px`.
fn range(f: &str, width: f32) -> Option<bool> {
    let mut parts: Vec<String> = Vec::new();
    let mut ops: Vec<String> = Vec::new();
    let mut cur = String::new();
    let chars: Vec<char> = f.chars().collect();
    let mut i = 0usize;
    while i < chars.len() {
        let c = chars[i];
        if matches!(c, '<' | '>' | '=') {
            parts.push(std::mem::take(&mut cur).trim().to_string());
            let mut op = c.to_string();
            if c != '=' && chars.get(i + 1) == Some(&'=') {
                op.push('=');
                i += 1;
            }
            ops.push(op);
        } else {
            cur.push(c);
        }
        i += 1;
    }
    parts.push(cur.trim().to_string());
    if ops.is_empty() || parts.len() != ops.len() + 1 {
        return None;
    }
    let val = |p: &str| if p == "width" { Some(width) } else { length(p) };
    if !parts.iter().any(|p| p == "width") {
        return None;
    }
    for (k, op) in ops.iter().enumerate() {
        let (a, b) = (val(&parts[k])?, val(&parts[k + 1])?);
        let ok = match op.as_str() {
            "<" => a < b,
            "<=" => a <= b,
            ">" => a > b,
            ">=" => a >= b,
            "=" => (a - b).abs() < 0.01,
            _ => return None,
        };
        if !ok {
            return Some(false);
        }
    }
    Some(true)
}

/// A media-query length in CSS px. `em` and `rem` are the initial 16 px there,
/// per spec — not the font size of anything in the document.
fn length(v: &str) -> Option<f32> {
    let v = v.trim();
    let num = |s: &str| s.trim().parse::<f32>().ok();
    if let Some(n) = v.strip_suffix("px").and_then(num) {
        Some(n)
    } else if let Some(n) = v.strip_suffix("rem").and_then(num) {
        Some(n * 16.0)
    } else if let Some(n) = v.strip_suffix("em").and_then(num) {
        Some(n * 16.0)
    } else if let Some(n) = v.strip_suffix("pt").and_then(num) {
        Some(n * 4.0 / 3.0)
    } else {
        num(v).filter(|n| *n == 0.0)
    }
}

#[cfg(test)]
mod tests {
    use super::matches;

    #[test]
    fn mobile_branches_apply_to_a_bubble() {
        assert!(matches("only screen and (max-width: 600px)", 420.0));
        assert!(matches("screen and (max-width:599.98px)", 420.0));
        assert!(matches("(max-width: 37.5em)", 420.0));
        assert!(!matches("only screen and (min-width: 600px)", 420.0));
        assert!(matches("only screen and (min-width:480px)", 560.0));
        assert!(matches("all", 420.0));
        assert!(matches("(width <= 600px)", 420.0));
        assert!(matches("(400px < width <= 700px)", 420.0));
        assert!(!matches("(400px < width <= 700px)", 800.0));
    }

    #[test]
    fn what_we_cannot_know_is_false() {
        assert!(!matches("print", 420.0));
        assert!(!matches("only screen and (max-device-width: 480px)", 420.0));
        assert!(!matches("(prefers-color-scheme: dark)", 420.0));
        assert!(!matches("screen and (-webkit-min-device-pixel-ratio: 0)", 420.0));
        assert!(!matches("(max-width: 600px", 420.0));
        // …but a comma list is any-of, and `not` inverts a whole query.
        assert!(matches("only screen and (max-device-width: 480px), (max-width: 480px)", 420.0));
        assert!(matches("not print", 420.0));
        assert!(!matches("not screen and (max-width: 600px)", 420.0));
    }
}
