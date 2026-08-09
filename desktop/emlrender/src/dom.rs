//! HTML → DOM, plus the sanitising decisions that happen at walk time.
//!
//! We keep the parsed tree as-is (html5ever's error recovery already turns the
//! malformed soup real mail is made of into something tree-shaped) and instead
//! decide per-element, during layout, whether it contributes anything. That
//! keeps the "what do we drop" policy in one readable place: [`is_dropped`].

use html5ever::tendril::TendrilSink;
use html5ever::{parse_document, ParseOpts};
use markup5ever_rcdom::{Handle, NodeData, RcDom};

/// Parse a full document. Never fails: html5ever recovers from anything.
pub fn parse(html: &str) -> Handle {
    let dom = parse_document(RcDom::default(), ParseOpts::default()).one(html);
    dom.document
}

/// Lowercase tag name of an element node, or `""` for anything else.
pub fn tag(node: &Handle) -> &str {
    match &node.data {
        NodeData::Element { name, .. } => name.local.as_ref(),
        _ => "",
    }
}

/// Value of an attribute, case-insensitive on the name.
pub fn attr(node: &Handle, want: &str) -> Option<String> {
    let NodeData::Element { attrs, .. } = &node.data else {
        return None;
    };
    attrs
        .borrow()
        .iter()
        .find(|a| a.name.local.as_ref().eq_ignore_ascii_case(want))
        .map(|a| a.value.to_string())
}

/// Text content of a text node, or `None`.
pub fn text(node: &Handle) -> Option<String> {
    match &node.data {
        NodeData::Text { contents } => Some(contents.borrow().to_string()),
        _ => None,
    }
}

pub fn children(node: &Handle) -> Vec<Handle> {
    node.children.borrow().clone()
}

/// Elements whose subtree contributes nothing to a rendered mail.
///
/// `<style>` is dropped here because its *content* is harvested separately
/// (see [`crate::style::Stylesheet::collect`]) before layout runs.
pub fn is_dropped(tag: &str) -> bool {
    matches!(
        tag,
        "script"
            | "style"
            | "head"
            | "meta"
            | "link"
            | "title"
            | "base"
            | "iframe"
            | "frame"
            | "frameset"
            | "object"
            | "embed"
            | "applet"
            | "noscript"
            | "template"
            | "map"
            | "area"
            | "audio"
            | "video"
            | "svg"
            | "form"
            | "input"
            | "button"
            | "select"
            | "textarea"
            | "option"
    )
}

/// Concatenated text of every `<style>` element in the document, in order.
pub fn collect_style_text(root: &Handle) -> String {
    let mut out = String::new();
    walk_styles(root, &mut out);
    out
}

fn walk_styles(node: &Handle, out: &mut String) {
    if tag(node) == "style" {
        for c in children(node).iter() {
            if let Some(t) = text(c) {
                out.push_str(&t);
                out.push('\n');
            }
        }
        return;
    }
    for c in children(node).iter() {
        walk_styles(c, out);
    }
}
