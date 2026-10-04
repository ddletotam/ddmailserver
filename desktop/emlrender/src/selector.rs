//! CSS selectors: the part of Selectors Level 3 that mail templates lean on.
//!
//! Supported: type and universal selectors, `.class` (any number), `#id`,
//! attribute tests (`[a]`, `=`, `~=`, `|=`, `^=`, `$=`, `*=`, optional ` i`),
//! the structural pseudo-classes `:first-child` / `:last-child` /
//! `:only-child` / `:root`, `:link` / `:any-link`, and all four combinators
//! (descendant, `>`, `+`, `~`).
//!
//! Anything else — `:hover`, `::before`, `:not(…)`, escapes — rejects the
//! **whole** selector rather than matching a looser one: an interaction state
//! that never happens in a static bitmap, or a pseudo-element we would apply
//! to the element itself, is a wrong match, and a wrong match is worse than
//! none.
//!
//! Class, id and tag names compare ASCII-case-insensitively. Strictly that is
//! quirks mode only, but most mail renders in quirks mode, and this is what
//! the matcher always did.

use markup5ever_rcdom::{Handle, NodeData};
use std::rc::Rc;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Comb {
    Descendant,
    Child,
    Adjacent,
    Sibling,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum AttrOp {
    Exists,
    Equals,
    Includes,
    DashMatch,
    Prefix,
    Suffix,
    Substring,
}

#[derive(Debug)]
struct AttrSel {
    name: String,
    op: AttrOp,
    value: String,
    ci: bool,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Pseudo {
    FirstChild,
    LastChild,
    OnlyChild,
    Root,
    Link,
}

#[derive(Debug, Default)]
pub struct Compound {
    /// Lowercase; `None` for `*` or no type selector at all.
    pub tag: Option<String>,
    /// Lowercase.
    pub id: Option<String>,
    /// Lowercase.
    pub classes: Vec<String>,
    attrs: Vec<AttrSel>,
    pseudos: Vec<Pseudo>,
}

/// One complex selector, compounds left to right.
#[derive(Debug)]
pub struct Selector {
    compounds: Vec<Compound>,
    /// `combs[i]` sits between `compounds[i]` and `compounds[i + 1]`.
    combs: Vec<Comb>,
    /// `(ids, classes+attrs+pseudos, types)` packed one byte each, so it
    /// compares like the CSS triple.
    pub spec: u32,
}

/// The rightmost compound's most selective key — what the stylesheet indexes
/// rules under, so a node only tests rules that can possibly match it.
pub enum Key<'a> {
    Id(&'a str),
    Class(&'a str),
    Tag(&'a str),
    Any,
}

impl Selector {
    pub fn key(&self) -> Key<'_> {
        let Some(c) = self.compounds.last() else { return Key::Any };
        if let Some(id) = &c.id {
            Key::Id(id)
        } else if let Some(class) = c.classes.first() {
            Key::Class(class)
        } else if let Some(tag) = &c.tag {
            Key::Tag(tag)
        } else {
            Key::Any
        }
    }

    pub fn matches(&self, el: &Handle) -> bool {
        // Descendant combinators backtrack; on hostile input (thousands of
        // nested elements and `div div div div`) that is exponential. Past the
        // budget the answer is "no match", which is the safe one.
        let mut budget = 4096u32;
        let last = self.compounds.len() - 1;
        self.match_at(last, el, &mut budget)
    }

    fn match_at(&self, i: usize, el: &Handle, budget: &mut u32) -> bool {
        if *budget == 0 {
            return false;
        }
        *budget -= 1;
        if !compound_matches(&self.compounds[i], el) {
            return false;
        }
        if i == 0 {
            return true;
        }
        match self.combs[i - 1] {
            Comb::Child => parent_element(el).is_some_and(|p| self.match_at(i - 1, &p, budget)),
            Comb::Descendant => {
                let mut cur = parent_element(el);
                while let Some(p) = cur {
                    if self.match_at(i - 1, &p, budget) {
                        return true;
                    }
                    if *budget == 0 {
                        return false;
                    }
                    cur = parent_element(&p);
                }
                false
            }
            Comb::Adjacent => {
                prev_element_siblings(el).first().is_some_and(|s| self.match_at(i - 1, s, budget))
            }
            Comb::Sibling => {
                prev_element_siblings(el).iter().any(|s| self.match_at(i - 1, s, budget))
            }
        }
    }
}

/// Parse one complex selector (no commas). `None` for anything unsupported.
pub fn parse(src: &str) -> Option<Selector> {
    let s: Vec<char> = src.trim().chars().collect();
    if s.is_empty() || s.contains(&'\\') {
        return None;
    }
    let mut compounds = Vec::new();
    let mut combs = Vec::new();
    let mut i = 0usize;
    let mut pending: Option<Comb> = None;
    loop {
        // Combinator (or plain whitespace = descendant) before the next compound.
        let mut saw_space = false;
        while i < s.len() && s[i].is_whitespace() {
            saw_space = true;
            i += 1;
        }
        if i >= s.len() {
            break;
        }
        let explicit = match s[i] {
            '>' => Some(Comb::Child),
            '+' => Some(Comb::Adjacent),
            '~' => Some(Comb::Sibling),
            _ => None,
        };
        if let Some(c) = explicit {
            if compounds.is_empty() || pending.is_some() {
                return None;
            }
            pending = Some(c);
            i += 1;
            continue;
        }
        if !compounds.is_empty() {
            match pending.take() {
                Some(c) => combs.push(c),
                None if saw_space => combs.push(Comb::Descendant),
                None => return None,
            }
        } else if pending.is_some() {
            return None;
        }
        let c = parse_compound(&s, &mut i)?;
        compounds.push(c);
    }
    if compounds.is_empty() || pending.is_some() || combs.len() + 1 != compounds.len() {
        return None;
    }
    let (mut a, mut b, mut c) = (0u32, 0u32, 0u32);
    for comp in &compounds {
        a += u32::from(comp.id.is_some());
        b += (comp.classes.len() + comp.attrs.len() + comp.pseudos.len()) as u32;
        c += u32::from(comp.tag.is_some());
    }
    let spec = a.min(255) << 16 | b.min(255) << 8 | c.min(255);
    Some(Selector { compounds, combs, spec })
}

fn is_ident(c: char) -> bool {
    c.is_alphanumeric() || c == '-' || c == '_' || !c.is_ascii()
}

fn ident(s: &[char], i: &mut usize) -> Option<String> {
    let start = *i;
    while *i < s.len() && is_ident(s[*i]) {
        *i += 1;
    }
    (*i > start).then(|| s[start..*i].iter().collect::<String>().to_ascii_lowercase())
}

fn parse_compound(s: &[char], i: &mut usize) -> Option<Compound> {
    let mut c = Compound::default();
    let start = *i;
    if s[*i] == '*' {
        *i += 1;
    } else if is_ident(s[*i]) {
        c.tag = Some(ident(s, i)?);
    }
    while *i < s.len() {
        match s[*i] {
            '.' => {
                *i += 1;
                c.classes.push(ident(s, i)?);
            }
            '#' => {
                *i += 1;
                let id = ident(s, i)?;
                if c.id.as_ref().is_some_and(|have| *have != id) {
                    return None; // `#a#b` never matches anything
                }
                c.id = Some(id);
            }
            '[' => {
                *i += 1;
                c.attrs.push(parse_attr(s, i)?);
            }
            ':' => {
                *i += 1;
                if s.get(*i) == Some(&':') {
                    return None; // pseudo-element
                }
                let name = ident(s, i)?;
                c.pseudos.push(match name.as_str() {
                    "first-child" => Pseudo::FirstChild,
                    "last-child" => Pseudo::LastChild,
                    "only-child" => Pseudo::OnlyChild,
                    "root" => Pseudo::Root,
                    "link" | "any-link" => Pseudo::Link,
                    _ => return None,
                });
            }
            c2 if c2.is_whitespace() || matches!(c2, '>' | '+' | '~') => break,
            _ => return None,
        }
    }
    (*i > start).then_some(c)
}

/// After `[`: `name`, optional operator and value, optional ` i`, then `]`.
fn parse_attr(s: &[char], i: &mut usize) -> Option<AttrSel> {
    let skip_ws = |i: &mut usize| {
        while *i < s.len() && s[*i].is_whitespace() {
            *i += 1;
        }
    };
    skip_ws(i);
    let name = ident(s, i)?;
    skip_ws(i);
    let op = match s.get(*i)? {
        ']' => {
            *i += 1;
            return Some(AttrSel { name, op: AttrOp::Exists, value: String::new(), ci: false });
        }
        '=' => AttrOp::Equals,
        c => {
            let op = match c {
                '~' => AttrOp::Includes,
                '|' => AttrOp::DashMatch,
                '^' => AttrOp::Prefix,
                '$' => AttrOp::Suffix,
                '*' => AttrOp::Substring,
                _ => return None,
            };
            *i += 1;
            if s.get(*i) != Some(&'=') {
                return None;
            }
            op
        }
    };
    *i += 1; // past '='
    skip_ws(i);
    let value = match s.get(*i)? {
        q @ ('"' | '\'') => {
            let q = *q;
            *i += 1;
            let start = *i;
            while *i < s.len() && s[*i] != q {
                *i += 1;
            }
            if *i >= s.len() {
                return None;
            }
            let v: String = s[start..*i].iter().collect();
            *i += 1;
            v
        }
        _ => {
            let start = *i;
            while *i < s.len() && is_ident(s[*i]) {
                *i += 1;
            }
            if *i == start {
                return None;
            }
            s[start..*i].iter().collect()
        }
    };
    skip_ws(i);
    let mut ci = false;
    if matches!(s.get(*i), Some('i' | 'I')) {
        ci = true;
        *i += 1;
        skip_ws(i);
    }
    if s.get(*i) != Some(&']') {
        return None;
    }
    *i += 1;
    Some(AttrSel { name, op, value, ci })
}

// ------------------------------------------------------------------ matching

fn compound_matches(c: &Compound, el: &Handle) -> bool {
    let NodeData::Element { name, attrs, .. } = &el.data else { return false };
    if c.tag.as_deref().is_some_and(|t| !name.local.as_ref().eq_ignore_ascii_case(t)) {
        return false;
    }
    let attrs = attrs.borrow();
    let get = |want: &str| {
        attrs.iter().find(|a| a.name.local.as_ref().eq_ignore_ascii_case(want)).map(|a| &*a.value)
    };
    if let Some(id) = &c.id {
        if !get("id").is_some_and(|v| v.trim().eq_ignore_ascii_case(id)) {
            return false;
        }
    }
    if !c.classes.is_empty() {
        let have = get("class").unwrap_or("");
        let has = |want: &String| have.split_whitespace().any(|h| h.eq_ignore_ascii_case(want));
        if !c.classes.iter().all(has) {
            return false;
        }
    }
    for a in &c.attrs {
        let Some(v) = get(&a.name) else { return false };
        if !attr_matches(a, v) {
            return false;
        }
    }
    for p in &c.pseudos {
        let ok = match p {
            Pseudo::Root => parent_element(el).is_none(),
            Pseudo::Link => matches!(name.local.as_ref(), "a" | "area") && get("href").is_some(),
            Pseudo::FirstChild => prev_element_siblings(el).is_empty(),
            Pseudo::LastChild => !has_next_element_sibling(el),
            Pseudo::OnlyChild => {
                prev_element_siblings(el).is_empty() && !has_next_element_sibling(el)
            }
        };
        if !ok {
            return false;
        }
    }
    true
}

fn attr_matches(a: &AttrSel, v: &str) -> bool {
    let (v, want) = if a.ci {
        (v.to_lowercase(), a.value.to_lowercase())
    } else {
        (v.to_string(), a.value.clone())
    };
    match a.op {
        AttrOp::Exists => true,
        AttrOp::Equals => v == want,
        AttrOp::Includes => v.split_whitespace().any(|w| w == want),
        AttrOp::DashMatch => v == want || v.starts_with(&format!("{want}-")),
        // Per spec, an empty value never matches the three substring tests.
        AttrOp::Prefix => !want.is_empty() && v.starts_with(&want),
        AttrOp::Suffix => !want.is_empty() && v.ends_with(&want),
        AttrOp::Substring => !want.is_empty() && v.contains(&want),
    }
}

fn is_element(n: &Handle) -> bool {
    matches!(n.data, NodeData::Element { .. })
}

/// The parent, if it is an element (the document node is not).
pub fn parent_element(el: &Handle) -> Option<Handle> {
    let weak = el.parent.take();
    let parent = weak.as_ref().and_then(|w| w.upgrade());
    el.parent.set(weak);
    parent.filter(is_element)
}

/// Element siblings before `el`, nearest first.
fn prev_element_siblings(el: &Handle) -> Vec<Handle> {
    let weak = el.parent.take();
    let parent = weak.as_ref().and_then(|w| w.upgrade());
    el.parent.set(weak);
    let Some(parent) = parent else { return Vec::new() };
    let kids = parent.children.borrow();
    let Some(at) = kids.iter().position(|k| Rc::ptr_eq(k, el)) else { return Vec::new() };
    kids[..at].iter().rev().filter(|k| is_element(k)).cloned().collect()
}

fn has_next_element_sibling(el: &Handle) -> bool {
    let weak = el.parent.take();
    let parent = weak.as_ref().and_then(|w| w.upgrade());
    el.parent.set(weak);
    let Some(parent) = parent else { return false };
    let kids = parent.children.borrow();
    let Some(at) = kids.iter().position(|k| Rc::ptr_eq(k, el)) else { return false };
    kids[at + 1..].iter().any(is_element)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn spec(s: &str) -> Option<(u32, u32, u32)> {
        parse(s).map(|p| (p.spec >> 16, p.spec >> 8 & 255, p.spec & 255))
    }

    #[test]
    fn parses_what_templates_write() {
        assert_eq!(spec("td"), Some((0, 0, 1)));
        assert_eq!(spec("*"), Some((0, 0, 0)));
        assert_eq!(spec(".a.b"), Some((0, 2, 0)));
        assert_eq!(spec("table[class=body] .x"), Some((0, 2, 1)));
        assert_eq!(spec("#outlook a"), Some((1, 0, 1)));
        assert_eq!(spec("div > p + span ~ b"), Some((0, 0, 4)));
        assert_eq!(spec("div>p"), Some((0, 0, 2)));
        assert_eq!(spec("a:link"), Some((0, 1, 1)));
        assert_eq!(spec("[data-ogsc] .x"), Some((0, 2, 0)));
        assert_eq!(spec("div[style*='margin: 16px 0']"), Some((0, 1, 1)));
    }

    #[test]
    fn rejects_what_it_cannot_honour() {
        for s in ["a:hover", "p::before", "p:not(.x)", "v\\:*", "> p", "p >", "p > > a", "[x", ""] {
            assert!(parse(s).is_none(), "{s}");
        }
    }
}
