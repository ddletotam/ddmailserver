//! Keyboard shortcuts matched by key, not by letter: Latin and Cyrillic
//! characters of the same physical key, and the virtual-key fallback for
//! other layouts (contract §3б).

/// Which editing action a key press means, on the keyboard layout in force
/// right now. 0 none, 1 copy, 2 paste, 3 cut, 4 select-all.
///
/// Slint hands the UI only the character a key produced, and its own shortcut
/// matcher compares that against the latin `"c"/"x"/"v"/"a"`
/// (`i-slint-core/input.rs`). On a non-latin layout the character is Cyrillic,
/// nothing matches, and neither the built-in clipboard nor anything written in
/// `.slint` fires. Enumerating layouts is not a fix — there are more than two,
/// and the list is never done.
///
/// So ask the OS the inverse question: *which key produces this character?*
/// `VkKeyScanEx` answers against the active layout, and the virtual key is a
/// property of the physical key rather than of what is printed on it.
pub(crate) fn shortcut_action(text: &str, held: bool) -> i32 {
    // Without a modifier this is someone typing, and every letter would
    // otherwise resolve to a virtual key and fire its shortcut.
    if !held {
        return 0;
    }
    shortcut_key(text).map_or(0, |vk| match vk {
        b'C' => 1,
        b'V' => 2,
        b'X' => 3,
        b'A' => 4,
        b'B' => 5, // жирный
        b'I' => 6, // курсив
        b'U' => 7, // подчёркнутый
        b'S' => 8, // зачёркнутый (X занят вырезанием, а резать текст в
        // композере нужнее, чем зачёркивать)
        _ => 0,
    })
}

/// Which letter key a press means, as an uppercase ASCII byte — decided
/// against the *current* keyboard layout, never against the letter printed on
/// the key.
///
/// Enumerating layouts does not work: there are more than two, and the list is
/// never finished. Ctrl+C on ЙЦУКЕН reports "с", on ΕΛΛΗΝΙΚΆ "ψ", and neither
/// Slint's own matcher (it compares against latin "c") nor a hand-written
/// table of pairs covers the next one.
///
/// Only for a press with Ctrl held — the caller checks that. Uncombined, every
/// letter typed would resolve to a key and fire its shortcut.
pub(crate) fn shortcut_key(text: &str) -> Option<u8> {
    let mut chars = text.chars();
    let ch = chars.next()?;
    if chars.next().is_some() {
        return None;
    }
    // Some backends send the control code Ctrl+letter produces (A→U+0001 …
    // Z→U+001A). It names the key outright, so take it. Note U+0009 is both
    // Tab and Ctrl+I: reached only with the modifier down, so plain Tab is
    // untouched.
    let code = ch as u32;
    if (1..=26).contains(&code) {
        return Some(b'A' + code as u8 - 1);
    }
    let mut utf16 = [0u16; 2];
    if ch.encode_utf16(&mut utf16).len() != 1 {
        return None; // outside the BMP: never a shortcut key
    }
    virtual_key(utf16[0]).or_else(|| cyrillic_key(ch))
}

/// Last resort for the one case where nothing can be asked: a Wayland-only
/// session, where the keymap belongs to the compositor and the client is not
/// told which key produced the character. Everywhere else this is dead code —
/// Windows answers with `VkKeyScanExW`, X11 with the keyboard mapping (see
/// `keylayout`). It is kept, and deliberately not extended: a table of letters
/// per language is never finished, and the two real answers above are.
pub(crate) fn cyrillic_key(ch: char) -> Option<u8> {
    let lower = ch.to_lowercase().next().unwrap_or(ch);
    Some(match lower {
        'с' => b'C',
        'м' => b'V',
        'ч' => b'X',
        'ф' => b'A',
        'и' => b'B',
        'ш' => b'I',
        'г' => b'U',
        'ы' => b'S',
        'я' => b'Z',
        'н' => b'Y',
        'л' => b'K',
        _ => return None,
    })
}

/// The virtual key that produces `ch` on the active layout.
#[cfg(windows)]
pub(crate) fn virtual_key(ch: u16) -> Option<u8> {
    // Declared here rather than through the `windows` crate: two calls, and
    // pulling in another feature of that crate for them is not worth it.
    unsafe extern "system" {
        fn GetKeyboardLayout(thread_id: u32) -> isize;
        fn VkKeyScanExW(ch: u16, layout: isize) -> i16;
    }
    // Thread 0 means "the foreground thread's layout", which is the one the
    // user is typing on.
    let scan = unsafe { VkKeyScanExW(ch, GetKeyboardLayout(0)) };
    if scan == -1 {
        return None;
    }
    Some((scan & 0xFF) as u8)
}

/// Elsewhere: the latin letter if the layout produced one, otherwise ask the
/// X server which physical key it was (`keylayout`).
///
/// The latin case is answered without touching X — a latin-only session never
/// opens a connection, and the common keystroke costs nothing.
#[cfg(not(windows))]
pub(crate) fn virtual_key(ch: u16) -> Option<u8> {
    let ch = char::from_u32(ch as u32)?;
    if ch.is_ascii_alphabetic() {
        return Some(ch.to_ascii_uppercase() as u8);
    }
    #[cfg(all(unix, not(target_os = "macos")))]
    {
        return crate::keylayout::latin_key(ch);
    }
    #[cfg(not(all(unix, not(target_os = "macos"))))]
    {
        let _ = ch;
        None
    }
}
