//! Which physical key produced a character — asked of the X server, not
//! guessed from a table of letters.
//!
//! Slint matches shortcuts by the character of the event, and the character is
//! whatever the active layout produced: Ctrl+C on ЙЦУКЕН arrives as "с", on
//! ΕΛΛΗΝΙΚΆ as "ψ". Windows answers the inverse question with `VkKeyScanExW`
//! ("which virtual key makes this character?") and that is the whole fix
//! there. On Linux there was no equivalent, so the code fell back to a
//! hand-written list of Cyrillic letters — which is the approach its own
//! comment called impossible to finish, and it was: the list covered eleven
//! letters and only one language.
//!
//! The X server already knows the answer. The core protocol's keyboard mapping
//! is a table of keycode → the keysyms that keycode can produce, across every
//! configured group. Find the keycode whose table contains the character, and
//! the keycode names the physical key regardless of what is printed on it.
//!
//! Deliberately the *physical position* and not "the keysym of group 1":
//! a session configured with Russian alone has no latin group to read, and the
//! position of the C key is the same on every PC keyboard whether or not any
//! layout admits it.

use std::sync::Mutex;
use std::time::{Duration, Instant};

use x11rb::connection::Connection;
use x11rb::protocol::xproto::ConnectionExt;
use x11rb::rust_connection::RustConnection;
use xkeysym::Keysym;

/// The letter engraved at each physical position on a PC keyboard, by X
/// keycode (evdev code + 8). Three letter rows, nothing else: these are the
/// only positions shortcuts are bound to, and a table of positions — unlike a
/// table of characters — is finished the moment it is written.
fn physical_letter(keycode: u8) -> Option<u8> {
    Some(match keycode {
        24 => b'Q',
        25 => b'W',
        26 => b'E',
        27 => b'R',
        28 => b'T',
        29 => b'Y',
        30 => b'U',
        31 => b'I',
        32 => b'O',
        33 => b'P',
        38 => b'A',
        39 => b'S',
        40 => b'D',
        41 => b'F',
        42 => b'G',
        43 => b'H',
        44 => b'J',
        45 => b'K',
        46 => b'L',
        52 => b'Z',
        53 => b'X',
        54 => b'C',
        55 => b'V',
        56 => b'B',
        57 => b'N',
        58 => b'M',
        _ => return None,
    })
}

/// A snapshot of the server's keycode → keysyms table.
struct Keymap {
    min_keycode: u8,
    per_keycode: usize,
    keysyms: Vec<u32>,
    fetched: Instant,
}

impl Keymap {
    /// The keycode that can produce `ch` in some group, if any.
    fn keycode_for(&self, ch: char) -> Option<u8> {
        if self.per_keycode == 0 {
            return None;
        }
        let wanted = lower(ch);
        self.keysyms.iter().enumerate().find_map(|(i, &sym)| {
            if sym == 0 {
                return None;
            }
            let produced = Keysym::new(sym).key_char()?;
            if lower(produced) != wanted {
                return None;
            }
            u8::try_from(self.min_keycode as usize + i / self.per_keycode).ok()
        })
    }
}

fn lower(ch: char) -> char {
    ch.to_lowercase().next().unwrap_or(ch)
}

/// How long a fetched mapping is trusted before a miss is allowed to refetch.
///
/// The mapping only changes when layouts are added or removed — switching
/// between configured groups does not touch it, because every group's keysyms
/// are in the table at once. So this exists purely so that adding a layout
/// mid-session starts working without a restart, and it is a floor on refetch
/// rate rather than a cache expiry: a miss is the common case (every Ctrl+key
/// that is not a shortcut misses), and refetching on each of those would put
/// an X round-trip in the keystroke path.
const REFETCH_AFTER: Duration = Duration::from_secs(5);

fn state() -> &'static Mutex<Option<(RustConnection, Option<Keymap>)>> {
    static STATE: std::sync::OnceLock<Mutex<Option<(RustConnection, Option<Keymap>)>>> =
        std::sync::OnceLock::new();
    STATE.get_or_init(|| Mutex::new(None))
}

fn fetch(conn: &RustConnection) -> Option<Keymap> {
    let setup = conn.setup();
    let min = setup.min_keycode;
    let count = setup.max_keycode.checked_sub(min)?.checked_add(1)?;
    let reply = conn.get_keyboard_mapping(min, count).ok()?.reply().ok()?;
    Some(Keymap {
        min_keycode: min,
        per_keycode: reply.keysyms_per_keycode as usize,
        keysyms: reply.keysyms,
        fetched: Instant::now(),
    })
}

/// The letter key that produced `ch` on the active layout, as an uppercase
/// ASCII byte. `None` when there is no X server to ask (a Wayland-only
/// session), or when no key produces this character.
pub fn latin_key(ch: char) -> Option<u8> {
    let mut guard = state().lock().ok()?;

    if guard.is_none() {
        // One connection for the process, opened on the first non-latin
        // shortcut and never on a latin-only session.
        let (conn, _) = x11rb::connect(None).ok()?;
        let map = fetch(&conn);
        *guard = Some((conn, map));
    }

    let (conn, map) = guard.as_mut()?;

    if let Some(found) = map.as_ref().and_then(|m| m.keycode_for(ch)) {
        return physical_letter(found);
    }

    // A miss can mean the character genuinely is not on the keyboard, or that
    // a layout was added since the snapshot. Refetch at most every few
    // seconds, then answer from the fresh table.
    let stale = map
        .as_ref()
        .is_none_or(|m| m.fetched.elapsed() >= REFETCH_AFTER);
    if stale {
        *map = fetch(conn);
        return map
            .as_ref()
            .and_then(|m| m.keycode_for(ch))
            .and_then(physical_letter);
    }

    None
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A keymap shaped like a real `us,ru` session: four keysyms per keycode
    /// (two groups × two shift levels), C at its usual position.
    fn us_ru_keymap() -> Keymap {
        let mut keysyms = vec![0u32; (58 - 24 + 1) * 4];
        let mut put = |keycode: u8, syms: [u32; 4]| {
            let base = (keycode as usize - 24) * 4;
            keysyms[base..base + 4].copy_from_slice(&syms);
        };
        // C: latin c/C in group 1, Cyrillic es/ES in group 2.
        put(54, [0x63, 0x43, 0x6d3, 0x6f3]);
        // V: latin v/V, Cyrillic em.
        put(55, [0x76, 0x56, 0x6cd, 0x6ed]);
        // A: latin a/A, Cyrillic ef.
        put(38, [0x61, 0x41, 0x6c6, 0x6e6]);
        Keymap {
            min_keycode: 24,
            per_keycode: 4,
            keysyms,
            fetched: Instant::now(),
        }
    }

    #[test]
    fn finds_the_key_behind_a_cyrillic_character() {
        let map = us_ru_keymap();
        // "с" is Cyrillic es — the character Ctrl+C produces on ЙЦУКЕН.
        assert_eq!(map.keycode_for('с').and_then(physical_letter), Some(b'C'));
        assert_eq!(map.keycode_for('м').and_then(physical_letter), Some(b'V'));
        assert_eq!(map.keycode_for('ф').and_then(physical_letter), Some(b'A'));
    }

    #[test]
    fn finds_the_key_behind_a_latin_character() {
        let map = us_ru_keymap();
        assert_eq!(map.keycode_for('c').and_then(physical_letter), Some(b'C'));
    }

    #[test]
    fn case_does_not_matter() {
        let map = us_ru_keymap();
        // Shift+Ctrl+C reports the capital; the key is the same key.
        assert_eq!(map.keycode_for('С').and_then(physical_letter), Some(b'C'));
        assert_eq!(map.keycode_for('C').and_then(physical_letter), Some(b'C'));
    }

    #[test]
    fn unknown_character_is_not_a_key() {
        let map = us_ru_keymap();
        assert_eq!(map.keycode_for('漢'), None);
    }

    /// The point of the rewrite: a layout nobody wrote a table for.
    #[test]
    fn works_for_a_layout_no_table_covers() {
        let mut keysyms = vec![0u32; (58 - 24 + 1) * 4];
        // Greek ψ sits on the C key in the Greek layout.
        let base = (54 - 24) * 4;
        keysyms[base..base + 4].copy_from_slice(&[0x63, 0x43, 0x7f8, 0x7d8]);
        let map = Keymap {
            min_keycode: 24,
            per_keycode: 4,
            keysyms,
            fetched: Instant::now(),
        };
        assert_eq!(map.keycode_for('ψ').and_then(physical_letter), Some(b'C'));
    }

    /// Against the real X server, whatever it is configured with. Ignored by
    /// default — it needs a session, and CI has none.
    ///
    /// Layout-agnostic on purpose: it asserts the round trip rather than any
    /// particular alphabet. Take each letter position, ask the server what
    /// characters that key can produce, and check that every one of them
    /// resolves back to the same position.
    #[test]
    #[ignore = "needs a live X session"]
    fn round_trips_against_the_live_server() {
        let Ok((conn, _)) = x11rb::connect(None) else {
            panic!("no X server to test against");
        };
        let map = fetch(&conn).expect("keyboard mapping");

        let mut checked = 0;
        for keycode in 24u8..=58 {
            let Some(expected) = physical_letter(keycode) else {
                continue;
            };
            let base = (keycode as usize - map.min_keycode as usize) * map.per_keycode;
            for &sym in &map.keysyms[base..base + map.per_keycode] {
                let Some(ch) = Keysym::new(sym).key_char() else {
                    continue;
                };
                if !ch.is_alphabetic() {
                    continue;
                }
                assert_eq!(
                    latin_key(ch),
                    Some(expected),
                    "{ch:?} (keysym {sym:#x}) sits on the {} key",
                    expected as char
                );
                checked += 1;
            }
        }
        assert!(checked >= 26, "only {checked} characters checked");
    }

    #[test]
    fn non_letter_positions_are_not_shortcut_keys() {
        // Space, digits, punctuation: real keycodes, but nothing binds them.
        assert_eq!(physical_letter(65), None); // space
        assert_eq!(physical_letter(10), None); // digit 1
    }
}
