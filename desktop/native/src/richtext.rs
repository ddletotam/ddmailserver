//! Модель rich-text документа композера: блоки, прогоны со стилем, правки,
//! отмена и сериализация в text/plain + text/html.
//!
//! Почему свой редактор, а не `TextInput`: слинтовский ввод — один стиль на всё
//! поле, встроить в него жирный/курсив/ссылку/картинку нельзя. Раскладку и
//! растеризацию делает [`crate::richtext_render`] (cosmic-text), этот файл —
//! чистая модель без единой зависимости от UI: его можно (и нужно) тестировать
//! без окна.
//!
//! Структура документа намеренно плоская — список блоков:
//!
//! * `Block::Para` — абзац, внутри прогоны (`Run`) с инлайн-стилем;
//! * `Block::Image` — картинка из буфера обмена, всегда своим блоком.
//!
//! Картинка отдельным блоком, а не инлайн-объектом в строке текста: cosmic-text
//! умеет верстать только текст, и «дырка» под картинку внутри строки потребовала
//! бы своего разбиения строк поверх шейпера. Телеграмное поведение (вставил
//! картинку — она встала своей строкой) при этом сохраняется, а модель остаётся
//! обозримой.
//!
//! Позиция каретки — `(индекс блока, байтовое смещение в plain-тексте блока)`.
//! Смещения байтовые (не по символам), потому что весь разбор текста ниже —
//! срезы `&str`; все точки входа обязаны попадать на границу символа
//! (см. [`Editor::clamp_pos`]).

use std::sync::Arc;

/// Инлайн-стиль прогона. Ссылка живёт отдельным полем ([`Run::link`]), а не
/// флагом стиля — у неё есть значение (href).
#[derive(Clone, Copy, Default, PartialEq, Eq, Debug)]
pub struct Style {
    pub bold: bool,
    pub italic: bool,
    pub underline: bool,
}

/// Что переключает тулбар / Ctrl+B,I,U.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum StyleBit {
    Bold,
    Italic,
    Underline,
}

impl Style {
    pub fn get(&self, bit: StyleBit) -> bool {
        match bit {
            StyleBit::Bold => self.bold,
            StyleBit::Italic => self.italic,
            StyleBit::Underline => self.underline,
        }
    }
    pub fn set(&mut self, bit: StyleBit, on: bool) {
        match bit {
            StyleBit::Bold => self.bold = on,
            StyleBit::Italic => self.italic = on,
            StyleBit::Underline => self.underline = on,
        }
    }
}

/// Кусок текста с одним стилем. Соседние прогоны с одинаковым стилем и ссылкой
/// склеиваются ([`normalize`]) — иначе набор по одной букве плодил бы по
/// прогону на символ.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Run {
    pub text: String,
    pub style: Style,
    pub link: Option<String>,
}

impl Run {
    fn same_format(&self, other: &Run) -> bool {
        self.style == other.style && self.link == other.link
    }
}

/// Картинка, вставленная из буфера обмена. Байты уже в отправляемом виде
/// (PNG/JPEG), `cid` — то, чем HTML-тело сошлётся на inline-вложение
/// (`<img src="cid:…">`), см. `OutgoingAttachment::content_id`.
///
/// `bytes` под `Arc`: снимки для отмены клонируют документ целиком, а картинка
/// из буфера — это мегабайты.
#[derive(Clone, Debug)]
pub struct InlineImage {
    pub cid: String,
    pub mime: String,
    pub bytes: Arc<Vec<u8>>,
    pub w: u32,
    pub h: u32,
}

#[derive(Clone, Debug)]
pub enum Block {
    Para(Vec<Run>),
    Image(InlineImage),
}

impl Block {
    /// Длина блока в байтах plain-текста. У картинки — 0: каретка стоит
    /// «на блоке», внутрь идти некуда.
    pub fn len(&self) -> usize {
        match self {
            Block::Para(runs) => runs.iter().map(|r| r.text.len()).sum(),
            Block::Image(_) => 0,
        }
    }
    pub fn text(&self) -> String {
        match self {
            Block::Para(runs) => runs.iter().map(|r| r.text.as_str()).collect(),
            Block::Image(_) => String::new(),
        }
    }
    pub fn is_image(&self) -> bool {
        matches!(self, Block::Image(_))
    }
}

/// Позиция каретки. `Ord` — лексикографический по (блок, смещение), на нём
/// держится нормализация выделения.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Debug, Default)]
pub struct Pos {
    pub block: usize,
    pub off: usize,
}

#[derive(Clone, Debug)]
pub struct Doc {
    pub blocks: Vec<Block>,
}

impl Default for Doc {
    fn default() -> Self {
        Doc { blocks: vec![Block::Para(Vec::new())] }
    }
}

/// Куда двигать каретку. Вертикальные шаги (`Up`/`Down`) модель не знает —
/// они зависят от переносов строк, их считает слой раскладки и присылает уже
/// готовую позицию через [`Editor::set_caret`].
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Motion {
    Left,
    Right,
    WordLeft,
    WordRight,
    LineStart,
    LineEnd,
    DocStart,
    DocEnd,
}

#[derive(Clone)]
struct Snapshot {
    blocks: Vec<Block>,
    caret: Pos,
    anchor: Pos,
}

/// Глубина истории отмены. Снимок — клон документа (картинки под `Arc`, так
/// что клон дешёвый), 100 шагов хватает и не разрастается.
const UNDO_DEPTH: usize = 100;

pub struct Editor {
    doc: Doc,
    caret: Pos,
    /// Второй конец выделения. `anchor == caret` — выделения нет.
    anchor: Pos,
    /// Стиль для следующего ввода при схлопнутой каретке: Ctrl+B «на пустом
    /// месте» должен включить жирный для того, что напечатают дальше.
    /// Сбрасывается любым перемещением каретки.
    pending: Option<Style>,
    undo: Vec<Snapshot>,
    redo: Vec<Snapshot>,
}

impl Default for Editor {
    fn default() -> Self {
        Editor {
            doc: Doc::default(),
            caret: Pos::default(),
            anchor: Pos::default(),
            pending: None,
            undo: Vec::new(),
            redo: Vec::new(),
        }
    }
}

impl Editor {
    pub fn new() -> Self {
        Self::default()
    }

    /// Редактор с уже набранным plain-текстом (восстановление черновика после
    /// неудачной отправки).
    pub fn from_text(text: &str) -> Self {
        let mut ed = Editor::new();
        if !text.is_empty() {
            ed.insert_str(text);
        }
        ed.undo.clear();
        ed
    }

    pub fn doc(&self) -> &Doc {
        &self.doc
    }
    pub fn caret(&self) -> Pos {
        self.caret
    }
    pub fn has_selection(&self) -> bool {
        self.caret != self.anchor
    }
    /// Выделение в нормальном порядке (начало ≤ конец).
    pub fn selection(&self) -> (Pos, Pos) {
        if self.anchor <= self.caret { (self.anchor, self.caret) } else { (self.caret, self.anchor) }
    }

    /// Пустой документ — ни текста, ни картинок. По нему решается, есть ли что
    /// отправлять (одна картинка без текста — есть).
    pub fn is_empty(&self) -> bool {
        self.doc.blocks.iter().all(|b| match b {
            Block::Para(runs) => runs.iter().all(|r| r.text.is_empty()),
            Block::Image(_) => false,
        })
    }

    pub fn clear(&mut self) {
        self.push_undo();
        self.doc = Doc::default();
        self.caret = Pos::default();
        self.anchor = Pos::default();
        self.pending = None;
    }

    // ── Чтение ─────────────────────────────────────────────────────────────

    /// text/plain-версия тела. Картинки в ней не представлены ничем — MUA
    /// покажет html-альтернативу; текстовая остаётся честным «что написали».
    pub fn plain_text(&self) -> String {
        let mut out = String::new();
        for (i, b) in self.doc.blocks.iter().enumerate() {
            if i > 0 {
                out.push('\n');
            }
            out.push_str(&b.text());
        }
        out
    }

    /// Все inline-картинки документа в порядке появления — отправка вешает их
    /// вложениями с `content_id`.
    pub fn images(&self) -> Vec<InlineImage> {
        self.doc
            .blocks
            .iter()
            .filter_map(|b| match b {
                Block::Image(img) => Some(img.clone()),
                Block::Para(_) => None,
            })
            .collect()
    }

    /// Стиль, который получит следующий ввод: явно взведённый `pending` либо
    /// стиль текста слева от каретки. Им же подсвечиваются кнопки тулбара.
    pub fn current_style(&self) -> Style {
        if let Some(p) = self.pending {
            return p;
        }
        if self.has_selection() {
            // У выделения стиль «общий», если он одинаков во всех прогонах;
            // иначе — пустой (кнопки погашены).
            let (s, e) = self.selection();
            let mut it = self.styles_in(s, e).into_iter();
            let first = it.next().unwrap_or_default();
            return if it.all(|st| st == first) { first } else { Style::default() };
        }
        self.style_before_caret()
    }

    fn style_before_caret(&self) -> Style {
        let Some(Block::Para(runs)) = self.doc.blocks.get(self.caret.block) else {
            return Style::default();
        };
        let mut acc = 0usize;
        let mut last = Style::default();
        for r in runs {
            if self.caret.off <= acc {
                break;
            }
            last = r.style;
            acc += r.text.len();
        }
        last
    }

    fn styles_in(&self, s: Pos, e: Pos) -> Vec<Style> {
        let mut out = Vec::new();
        for bi in s.block..=e.block.min(self.doc.blocks.len().saturating_sub(1)) {
            let Some(Block::Para(runs)) = self.doc.blocks.get(bi) else { continue };
            let from = if bi == s.block { s.off } else { 0 };
            let to = if bi == e.block { e.off } else { usize::MAX };
            let mut acc = 0usize;
            for r in runs {
                let (rs, re) = (acc, acc + r.text.len());
                acc = re;
                if re > from && rs < to && !r.text.is_empty() {
                    out.push(r.style);
                }
            }
        }
        out
    }

    /// Plain-текст выделения — то, что уходит в буфер по Ctrl+C.
    pub fn selection_text(&self) -> String {
        if !self.has_selection() {
            return String::new();
        }
        let (s, e) = self.selection();
        let mut out = String::new();
        for bi in s.block..=e.block {
            let Some(b) = self.doc.blocks.get(bi) else { break };
            if bi > s.block {
                out.push('\n');
            }
            let text = b.text();
            let from = if bi == s.block { s.off.min(text.len()) } else { 0 };
            let to = if bi == e.block { e.off.min(text.len()) } else { text.len() };
            if from <= to {
                out.push_str(&text[from..to]);
            }
        }
        out
    }

    // ── Правки ─────────────────────────────────────────────────────────────

    /// Вставка текста в позицию каретки. Переводы строк режут на абзацы —
    /// вставка многострочного текста из буфера даёт несколько блоков, а не
    /// «\n» внутри одного.
    pub fn insert_str(&mut self, text: &str) {
        if text.is_empty() {
            return;
        }
        self.push_undo();
        self.delete_selection_inner();
        let style = self.pending.unwrap_or_else(|| self.style_before_caret());
        let link = self.link_before_caret();
        let normalized = text.replace("\r\n", "\n").replace('\r', "\n");
        let mut parts = normalized.split('\n');
        let first = parts.next().unwrap_or("");
        self.insert_inline(first, style, link.clone());
        for part in parts {
            self.split_block_inner();
            self.insert_inline(part, style, link.clone());
        }
        self.pending = None;
    }

    /// Вставка ссылки: URL из буфера поверх выделения превращает выделенный
    /// текст в ссылку (как в любом почтовике/редакторе), а на пустой каретке
    /// вставляет сам URL как ссылку.
    pub fn insert_link(&mut self, href: &str) {
        if href.is_empty() {
            return;
        }
        if self.has_selection() {
            self.push_undo();
            let (s, e) = self.selection();
            self.map_runs_in(s, e, |r| r.link = Some(href.to_string()));
            self.normalize_all();
            return;
        }
        self.push_undo();
        let style = self.pending.unwrap_or_else(|| self.style_before_caret());
        self.insert_inline(href, style, Some(href.to_string()));
        self.pending = None;
    }

    pub fn insert_image(&mut self, img: InlineImage) {
        self.push_undo();
        self.delete_selection_inner();
        // Картинка встаёт своим блоком: разрезаем текущий абзац по каретке и
        // вклиниваемся между половинами. Пустой хвост не плодим — если справа
        // от каретки ничего нет, каретка просто встаёт за картинкой.
        let at = self.caret;
        let tail_empty = match self.doc.blocks.get(at.block) {
            Some(Block::Para(runs)) => at.off >= runs.iter().map(|r| r.text.len()).sum::<usize>(),
            _ => true,
        };
        if !tail_empty {
            self.split_block_inner();
            self.doc.blocks.insert(at.block + 1, Block::Image(img));
            self.caret = Pos { block: at.block + 2, off: 0 };
        } else {
            // Каретка в конце блока (или на картинке) — новая картинка встаёт
            // следующим блоком.
            let idx = at.block + 1;
            self.doc.blocks.insert(idx, Block::Image(img));
            // После картинки всегда должен быть абзац — иначе некуда печатать.
            if self.doc.blocks.get(idx + 1).map(|b| b.is_image()).unwrap_or(true) {
                self.doc.blocks.insert(idx + 1, Block::Para(Vec::new()));
            }
            self.caret = Pos { block: idx + 1, off: 0 };
        }
        self.anchor = self.caret;
        self.pending = None;
    }

    /// Enter: разрыв абзаца.
    pub fn split_block(&mut self) {
        self.push_undo();
        self.delete_selection_inner();
        self.split_block_inner();
        self.pending = None;
    }

    pub fn backspace(&mut self) {
        if self.has_selection() {
            self.push_undo();
            self.delete_selection_inner();
            return;
        }
        self.push_undo();
        let at = self.caret;
        if at.off > 0 {
            let prev = self.prev_char_boundary(at.block, at.off);
            self.remove_range(Pos { block: at.block, off: prev }, at);
            return;
        }
        if at.block == 0 {
            self.undo.pop(); // нечего удалять — не оставляем пустой шаг отмены
            return;
        }
        // Начало блока: съедаем картинку слева целиком, либо склеиваем абзацы.
        let prev_block = at.block - 1;
        if self.doc.blocks[prev_block].is_image() {
            self.doc.blocks.remove(prev_block);
            self.caret = Pos { block: prev_block, off: 0 };
            self.anchor = self.caret;
            self.ensure_not_empty();
            return;
        }
        if self.doc.blocks[at.block].is_image() {
            let len = self.doc.blocks[prev_block].len();
            self.doc.blocks.remove(at.block);
            self.caret = Pos { block: prev_block, off: len };
            self.anchor = self.caret;
            self.ensure_not_empty();
            return;
        }
        let join_at = self.doc.blocks[prev_block].len();
        self.join_with_next(prev_block);
        self.caret = Pos { block: prev_block, off: join_at };
        self.anchor = self.caret;
    }

    pub fn delete_forward(&mut self) {
        if self.has_selection() {
            self.push_undo();
            self.delete_selection_inner();
            return;
        }
        self.push_undo();
        let at = self.caret;
        let len = self.doc.blocks.get(at.block).map(|b| b.len()).unwrap_or(0);
        if at.off < len {
            let next = self.next_char_boundary(at.block, at.off);
            self.remove_range(at, Pos { block: at.block, off: next });
            return;
        }
        if at.block + 1 >= self.doc.blocks.len() {
            self.undo.pop();
            return;
        }
        if self.doc.blocks[at.block + 1].is_image() {
            self.doc.blocks.remove(at.block + 1);
            self.ensure_not_empty();
            return;
        }
        if self.doc.blocks[at.block].is_image() {
            self.doc.blocks.remove(at.block);
            self.caret = Pos { block: at.block, off: 0 };
            self.anchor = self.caret;
            self.ensure_not_empty();
            return;
        }
        self.join_with_next(at.block);
    }

    pub fn delete_selection(&mut self) {
        if !self.has_selection() {
            return;
        }
        self.push_undo();
        self.delete_selection_inner();
    }

    /// Ctrl+B/I/U. С выделением — переключает его целиком (если весь фрагмент
    /// уже в этом стиле, снимает); без выделения — взводит `pending` для
    /// следующего ввода.
    pub fn toggle_style(&mut self, bit: StyleBit) {
        if !self.has_selection() {
            let mut st = self.current_style();
            st.set(bit, !st.get(bit));
            self.pending = Some(st);
            return;
        }
        self.push_undo();
        let (s, e) = self.selection();
        let all_on = self.styles_in(s, e).iter().all(|st| st.get(bit));
        self.map_runs_in(s, e, |r| r.style.set(bit, !all_on));
        self.normalize_all();
    }

    // ── Каретка и выделение ────────────────────────────────────────────────

    /// Прямая установка каретки (клик мышью, вертикальные шаги из слоя
    /// раскладки). `extend` — Shift-вариант: тянем выделение, не сбрасывая якорь.
    pub fn set_caret(&mut self, pos: Pos, extend: bool) {
        self.caret = self.clamp_pos(pos);
        if !extend {
            self.anchor = self.caret;
        }
        self.pending = None;
    }

    pub fn move_caret(&mut self, m: Motion, extend: bool) {
        // Схлопывание выделения стрелкой без Shift — к соответствующему краю,
        // а не «куда встала каретка»: так ведут себя все текстовые поля.
        if self.has_selection() && !extend && matches!(m, Motion::Left | Motion::Right) {
            let (s, e) = self.selection();
            self.caret = if m == Motion::Left { s } else { e };
            self.anchor = self.caret;
            self.pending = None;
            return;
        }
        let at = self.caret;
        let new = match m {
            Motion::Left => {
                if at.off > 0 {
                    Pos { block: at.block, off: self.prev_char_boundary(at.block, at.off) }
                } else if at.block > 0 {
                    Pos { block: at.block - 1, off: self.doc.blocks[at.block - 1].len() }
                } else {
                    at
                }
            }
            Motion::Right => {
                let len = self.doc.blocks.get(at.block).map(|b| b.len()).unwrap_or(0);
                if at.off < len {
                    Pos { block: at.block, off: self.next_char_boundary(at.block, at.off) }
                } else if at.block + 1 < self.doc.blocks.len() {
                    Pos { block: at.block + 1, off: 0 }
                } else {
                    at
                }
            }
            Motion::WordLeft => self.word_boundary(at, false),
            Motion::WordRight => self.word_boundary(at, true),
            Motion::LineStart => Pos { block: at.block, off: 0 },
            Motion::LineEnd => {
                Pos { block: at.block, off: self.doc.blocks.get(at.block).map(|b| b.len()).unwrap_or(0) }
            }
            Motion::DocStart => Pos { block: 0, off: 0 },
            Motion::DocEnd => {
                let last = self.doc.blocks.len().saturating_sub(1);
                Pos { block: last, off: self.doc.blocks[last].len() }
            }
        };
        self.set_caret(new, extend);
    }

    pub fn select_all(&mut self) {
        let last = self.doc.blocks.len().saturating_sub(1);
        self.anchor = Pos { block: 0, off: 0 };
        self.caret = Pos { block: last, off: self.doc.blocks[last].len() };
        self.pending = None;
    }

    /// Двойной клик: выделить слово под позицией.
    pub fn select_word_at(&mut self, pos: Pos) {
        let pos = self.clamp_pos(pos);
        let text = self.doc.blocks.get(pos.block).map(|b| b.text()).unwrap_or_default();
        let is_word = |c: char| c.is_alphanumeric() || c == '_';
        let start = text[..pos.off]
            .char_indices()
            .rev()
            .take_while(|(_, c)| is_word(*c))
            .last()
            .map(|(i, _)| i)
            .unwrap_or(pos.off);
        let end = text[pos.off..]
            .char_indices()
            .take_while(|(_, c)| is_word(*c))
            .last()
            .map(|(i, c)| pos.off + i + c.len_utf8())
            .unwrap_or(pos.off);
        self.anchor = Pos { block: pos.block, off: start };
        self.caret = Pos { block: pos.block, off: end };
        self.pending = None;
    }

    // ── Отмена ─────────────────────────────────────────────────────────────

    pub fn undo(&mut self) {
        if let Some(snap) = self.undo.pop() {
            self.redo.push(self.snapshot());
            self.restore(snap);
        }
    }

    pub fn redo(&mut self) {
        if let Some(snap) = self.redo.pop() {
            self.undo.push(self.snapshot());
            self.restore(snap);
        }
    }

    fn snapshot(&self) -> Snapshot {
        Snapshot { blocks: self.doc.blocks.clone(), caret: self.caret, anchor: self.anchor }
    }

    fn restore(&mut self, snap: Snapshot) {
        self.doc.blocks = snap.blocks;
        self.caret = self.clamp_pos(snap.caret);
        self.anchor = self.clamp_pos(snap.anchor);
        self.pending = None;
    }

    fn push_undo(&mut self) {
        self.undo.push(self.snapshot());
        if self.undo.len() > UNDO_DEPTH {
            self.undo.remove(0);
        }
        self.redo.clear();
    }

    // ── Внутреннее ─────────────────────────────────────────────────────────

    /// Позиция внутри документа и на границе символа. Всё, что приходит извне
    /// (клик, снимок отмены, вертикальный шаг), проходит через это — иначе
    /// срез `&str` по середине UTF-8 символа паникует.
    fn clamp_pos(&self, mut p: Pos) -> Pos {
        if self.doc.blocks.is_empty() {
            return Pos::default();
        }
        p.block = p.block.min(self.doc.blocks.len() - 1);
        let text = self.doc.blocks[p.block].text();
        p.off = p.off.min(text.len());
        while p.off > 0 && !text.is_char_boundary(p.off) {
            p.off -= 1;
        }
        p
    }

    fn prev_char_boundary(&self, block: usize, off: usize) -> usize {
        let text = self.doc.blocks[block].text();
        text[..off].chars().next_back().map(|c| off - c.len_utf8()).unwrap_or(0)
    }

    fn next_char_boundary(&self, block: usize, off: usize) -> usize {
        let text = self.doc.blocks[block].text();
        text[off..].chars().next().map(|c| off + c.len_utf8()).unwrap_or(off)
    }

    fn word_boundary(&self, at: Pos, forward: bool) -> Pos {
        let text = self.doc.blocks.get(at.block).map(|b| b.text()).unwrap_or_default();
        let is_word = |c: char| c.is_alphanumeric() || c == '_';
        if forward {
            if at.off >= text.len() {
                return if at.block + 1 < self.doc.blocks.len() {
                    Pos { block: at.block + 1, off: 0 }
                } else {
                    at
                };
            }
            let mut off = at.off;
            let mut seen_word = false;
            for (i, c) in text[at.off..].char_indices() {
                let abs = at.off + i;
                if is_word(c) {
                    seen_word = true;
                } else if seen_word {
                    return Pos { block: at.block, off: abs };
                }
                off = abs + c.len_utf8();
            }
            Pos { block: at.block, off }
        } else {
            if at.off == 0 {
                return if at.block > 0 {
                    Pos { block: at.block - 1, off: self.doc.blocks[at.block - 1].len() }
                } else {
                    at
                };
            }
            let mut off = 0;
            let mut seen_word = false;
            for (i, c) in text[..at.off].char_indices().rev() {
                if is_word(c) {
                    seen_word = true;
                } else if seen_word {
                    off = i + c.len_utf8();
                    return Pos { block: at.block, off };
                }
                off = i;
            }
            Pos { block: at.block, off }
        }
    }

    fn link_before_caret(&self) -> Option<String> {
        // Ссылку продолжаем только при вводе ВНУТРИ неё: набор в конце ссылки
        // не должен утягивать в href новые буквы.
        let Some(Block::Para(runs)) = self.doc.blocks.get(self.caret.block) else { return None };
        let mut acc = 0usize;
        for r in runs {
            let (rs, re) = (acc, acc + r.text.len());
            acc = re;
            if self.caret.off > rs && self.caret.off < re {
                return r.link.clone();
            }
        }
        None
    }

    fn insert_inline(&mut self, text: &str, style: Style, link: Option<String>) {
        if text.is_empty() {
            return;
        }
        let at = self.caret;
        // В картинку печатать нельзя — заводим абзац следом за ней.
        if self.doc.blocks.get(at.block).map(|b| b.is_image()).unwrap_or(false) {
            self.doc.blocks.insert(at.block + 1, Block::Para(Vec::new()));
            self.caret = Pos { block: at.block + 1, off: 0 };
        }
        let at = self.caret;
        let Some(Block::Para(runs)) = self.doc.blocks.get_mut(at.block) else { return };
        let mut acc = 0usize;
        let mut idx = runs.len();
        let mut split_at = 0usize;
        for (i, r) in runs.iter().enumerate() {
            let re = acc + r.text.len();
            if at.off <= re {
                idx = i;
                split_at = at.off - acc;
                break;
            }
            acc = re;
        }
        let new_run = Run { text: text.to_string(), style, link };
        if idx >= runs.len() {
            runs.push(new_run);
        } else if split_at == 0 {
            runs.insert(idx, new_run);
        } else if split_at >= runs[idx].text.len() {
            runs.insert(idx + 1, new_run);
        } else {
            let tail = runs[idx].text.split_off(split_at);
            let tail_run = Run { text: tail, style: runs[idx].style, link: runs[idx].link.clone() };
            runs.insert(idx + 1, tail_run);
            runs.insert(idx + 1, new_run);
        }
        self.caret = Pos { block: at.block, off: at.off + text.len() };
        self.anchor = self.caret;
        self.normalize_block(at.block);
    }

    fn split_block_inner(&mut self) {
        let at = self.caret;
        match self.doc.blocks.get_mut(at.block) {
            Some(Block::Para(runs)) => {
                let mut acc = 0usize;
                let mut tail: Vec<Run> = Vec::new();
                let mut cut_idx = runs.len();
                let mut split_at = 0usize;
                for (i, r) in runs.iter().enumerate() {
                    let re = acc + r.text.len();
                    if at.off <= re {
                        cut_idx = i;
                        split_at = at.off - acc;
                        break;
                    }
                    acc = re;
                }
                if cut_idx < runs.len() {
                    if split_at < runs[cut_idx].text.len() {
                        let rest = runs[cut_idx].text.split_off(split_at);
                        tail.push(Run {
                            text: rest,
                            style: runs[cut_idx].style,
                            link: runs[cut_idx].link.clone(),
                        });
                    }
                    tail.extend(runs.drain(cut_idx + 1..));
                    if runs.get(cut_idx).map(|r| r.text.is_empty()).unwrap_or(false) {
                        runs.remove(cut_idx);
                    }
                }
                self.doc.blocks.insert(at.block + 1, Block::Para(tail));
            }
            // Enter на картинке — пустой абзац следом.
            Some(Block::Image(_)) => {
                self.doc.blocks.insert(at.block + 1, Block::Para(Vec::new()));
            }
            None => return,
        }
        self.caret = Pos { block: at.block + 1, off: 0 };
        self.anchor = self.caret;
    }

    fn delete_selection_inner(&mut self) {
        if !self.has_selection() {
            return;
        }
        let (s, e) = self.selection();
        self.remove_range(s, e);
    }

    /// Удаление диапазона (в т.ч. через границы блоков). Картинки, целиком
    /// попавшие в диапазон, исчезают вместе с ним.
    fn remove_range(&mut self, s: Pos, e: Pos) {
        if s == e {
            return;
        }
        if s.block == e.block {
            if let Some(Block::Para(runs)) = self.doc.blocks.get_mut(s.block) {
                cut_runs(runs, s.off, e.off);
            } else if self.doc.blocks.get(s.block).map(|b| b.is_image()).unwrap_or(false) {
                self.doc.blocks.remove(s.block);
            }
        } else {
            // Хвост последнего блока приклеивается к голове первого; всё, что
            // между ними, вырезается целиком.
            let tail: Vec<Run> = match self.doc.blocks.get_mut(e.block) {
                Some(Block::Para(runs)) => {
                    cut_runs(runs, 0, e.off);
                    std::mem::take(runs)
                }
                _ => Vec::new(),
            };
            let head_is_image = self.doc.blocks[s.block].is_image();
            self.doc.blocks.drain(s.block + 1..=e.block.min(self.doc.blocks.len() - 1));
            if head_is_image {
                // Начало диапазона — сама картинка: она уходит, а хвост
                // становится на её место.
                self.doc.blocks[s.block] = Block::Para(tail);
            } else if let Some(Block::Para(runs)) = self.doc.blocks.get_mut(s.block) {
                let len: usize = runs.iter().map(|r| r.text.len()).sum();
                cut_runs(runs, s.off.min(len), len);
                runs.extend(tail);
            }
        }
        self.caret = self.clamp_pos(s);
        self.anchor = self.caret;
        self.ensure_not_empty();
        self.normalize_block(self.caret.block);
    }

    fn join_with_next(&mut self, block: usize) {
        if block + 1 >= self.doc.blocks.len() {
            return;
        }
        let next = self.doc.blocks.remove(block + 1);
        if let (Some(Block::Para(runs)), Block::Para(tail)) =
            (self.doc.blocks.get_mut(block), next)
        {
            runs.extend(tail);
        }
        self.normalize_block(block);
    }

    /// Документ никогда не бывает пустым списком блоков — каретке нужно где-то
    /// стоять; и последним блоком не должна оставаться картинка (иначе некуда
    /// печатать после неё).
    fn ensure_not_empty(&mut self) {
        if self.doc.blocks.is_empty() {
            self.doc.blocks.push(Block::Para(Vec::new()));
        }
        if self.doc.blocks.last().map(|b| b.is_image()).unwrap_or(false) {
            self.doc.blocks.push(Block::Para(Vec::new()));
        }
        self.caret = self.clamp_pos(self.caret);
        self.anchor = self.clamp_pos(self.anchor);
    }

    /// Применить правку стиля/ссылки ко всем прогонам в диапазоне, разрезав
    /// пограничные прогоны по краям выделения.
    fn map_runs_in(&mut self, s: Pos, e: Pos, f: impl Fn(&mut Run) + Copy) {
        for bi in s.block..=e.block.min(self.doc.blocks.len().saturating_sub(1)) {
            let from = if bi == s.block { s.off } else { 0 };
            let Some(Block::Para(runs)) = self.doc.blocks.get_mut(bi) else { continue };
            let total: usize = runs.iter().map(|r| r.text.len()).sum();
            let to = if bi == e.block { e.off.min(total) } else { total };
            if from >= to {
                continue;
            }
            split_run_at(runs, from);
            split_run_at(runs, to);
            let mut acc = 0usize;
            for r in runs.iter_mut() {
                let (rs, re) = (acc, acc + r.text.len());
                acc = re;
                if rs >= from && re <= to {
                    f(r);
                }
            }
        }
    }

    fn normalize_block(&mut self, block: usize) {
        if let Some(Block::Para(runs)) = self.doc.blocks.get_mut(block) {
            normalize(runs);
        }
    }

    fn normalize_all(&mut self) {
        for b in self.doc.blocks.iter_mut() {
            if let Block::Para(runs) = b {
                normalize(runs);
            }
        }
    }
}

/// Склеить соседние прогоны одного формата и выбросить пустые.
fn normalize(runs: &mut Vec<Run>) {
    runs.retain(|r| !r.text.is_empty());
    let mut i = 0;
    while i + 1 < runs.len() {
        if runs[i].same_format(&runs[i + 1]) {
            let tail = runs.remove(i + 1).text;
            runs[i].text.push_str(&tail);
        } else {
            i += 1;
        }
    }
}

/// Разрезать прогон так, чтобы `off` попал на границу между прогонами.
fn split_run_at(runs: &mut Vec<Run>, off: usize) {
    let mut acc = 0usize;
    for i in 0..runs.len() {
        let (rs, re) = (acc, acc + runs[i].text.len());
        acc = re;
        if off > rs && off < re {
            let tail = runs[i].text.split_off(off - rs);
            let new = Run { text: tail, style: runs[i].style, link: runs[i].link.clone() };
            runs.insert(i + 1, new);
            return;
        }
    }
}

/// Вырезать байтовый диапазон из прогонов абзаца.
fn cut_runs(runs: &mut Vec<Run>, from: usize, to: usize) {
    if from >= to {
        return;
    }
    let mut acc = 0usize;
    for r in runs.iter_mut() {
        let (rs, re) = (acc, acc + r.text.len());
        acc = re;
        if re <= from || rs >= to {
            continue;
        }
        let cut_start = from.saturating_sub(rs).min(r.text.len());
        let cut_end = (to - rs).min(r.text.len());
        r.text.replace_range(cut_start..cut_end, "");
    }
    normalize(runs);
}

// ── Сериализация ───────────────────────────────────────────────────────────

fn escape_html(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        match c {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            _ => out.push(c),
        }
    }
    out
}

/// Первая позиция URL в тексте (http/https/www) и его длина — для
/// автоссылок: набранный руками адрес должен уйти кликабельным.
fn find_url(text: &str) -> Option<(usize, usize)> {
    const PREFIXES: [&str; 3] = ["https://", "http://", "www."];
    let mut best: Option<(usize, usize)> = None;
    for p in PREFIXES {
        let mut from = 0usize;
        while let Some(rel) = text[from..].find(p) {
            let start = from + rel;
            // Не цепляемся к середине слова (…foohttp://…).
            let ok = start == 0
                || !text[..start].chars().next_back().map(|c| c.is_alphanumeric()).unwrap_or(false);
            if ok {
                let end = text[start..]
                    .find(|c: char| c.is_whitespace() || c == '<' || c == '"')
                    .map(|i| start + i)
                    .unwrap_or(text.len());
                // Хвостовая пунктуация в адрес не входит.
                let end = text[start..end]
                    .char_indices()
                    .rev()
                    .find(|(_, c)| !matches!(c, '.' | ',' | ')' | ';' | ':' | '!' | '?'))
                    .map(|(i, c)| start + i + c.len_utf8())
                    .unwrap_or(start);
                if end > start && best.map(|(bs, _)| start < bs).unwrap_or(true) {
                    best = Some((start, end));
                }
            }
            from = start + p.len();
        }
    }
    best
}

/// Текст прогона в HTML: экранирование + автоссылки. Прогон, уже помеченный
/// ссылкой, отдаётся как есть — второй раз оборачивать нельзя.
fn run_inner_html(r: &Run) -> String {
    if r.link.is_some() {
        return escape_html(&r.text);
    }
    let mut out = String::new();
    let mut rest = r.text.as_str();
    while let Some((s, e)) = find_url(rest) {
        out.push_str(&escape_html(&rest[..s]));
        let url = &rest[s..e];
        let href = if url.starts_with("www.") { format!("http://{url}") } else { url.to_string() };
        out.push_str(&format!(
            "<a href=\"{}\">{}</a>",
            escape_html(&href),
            escape_html(url)
        ));
        rest = &rest[e..];
    }
    out.push_str(&escape_html(rest));
    out
}

impl Editor {
    /// text/html-версия тела. Фрагмент (без `<html>`) — SMTP-слой кладёт его
    /// в `text/html`-часть multipart/alternative, а картинки ссылаются на
    /// inline-вложения через `cid:`.
    pub fn html(&self) -> String {
        let mut out = String::new();
        for b in &self.doc.blocks {
            match b {
                Block::Para(runs) => {
                    if runs.iter().all(|r| r.text.is_empty()) {
                        out.push_str("<div><br></div>");
                        continue;
                    }
                    out.push_str("<div>");
                    for r in runs {
                        if r.text.is_empty() {
                            continue;
                        }
                        let mut open = String::new();
                        let mut close = String::new();
                        if let Some(href) = &r.link {
                            open.push_str(&format!("<a href=\"{}\">", escape_html(href)));
                            close.insert_str(0, "</a>");
                        }
                        if r.style.bold {
                            open.push_str("<b>");
                            close.insert_str(0, "</b>");
                        }
                        if r.style.italic {
                            open.push_str("<i>");
                            close.insert_str(0, "</i>");
                        }
                        if r.style.underline {
                            open.push_str("<u>");
                            close.insert_str(0, "</u>");
                        }
                        out.push_str(&open);
                        out.push_str(&run_inner_html(r));
                        out.push_str(&close);
                    }
                    out.push_str("</div>");
                }
                Block::Image(img) => {
                    out.push_str(&format!(
                        "<div><img src=\"cid:{}\" style=\"max-width:100%\"></div>",
                        escape_html(&img.cid)
                    ));
                }
            }
        }
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn img() -> InlineImage {
        InlineImage {
            cid: "img1@ddmail".into(),
            mime: "image/png".into(),
            bytes: Arc::new(vec![1, 2, 3]),
            w: 10,
            h: 10,
        }
    }

    #[test]
    fn insert_and_plain_text() {
        let mut ed = Editor::new();
        ed.insert_str("привет");
        assert_eq!(ed.plain_text(), "привет");
        assert_eq!(ed.caret(), Pos { block: 0, off: "привет".len() });
    }

    #[test]
    fn multiline_paste_splits_into_blocks() {
        let mut ed = Editor::new();
        ed.insert_str("раз\r\nдва\nтри");
        assert_eq!(ed.doc().blocks.len(), 3);
        assert_eq!(ed.plain_text(), "раз\nдва\nтри");
    }

    #[test]
    fn backspace_walks_utf8_chars() {
        let mut ed = Editor::new();
        ed.insert_str("мир");
        ed.backspace();
        assert_eq!(ed.plain_text(), "ми");
    }

    #[test]
    fn backspace_at_block_start_joins() {
        let mut ed = Editor::new();
        ed.insert_str("раз\nдва");
        ed.set_caret(Pos { block: 1, off: 0 }, false);
        ed.backspace();
        assert_eq!(ed.plain_text(), "раздва");
        assert_eq!(ed.caret(), Pos { block: 0, off: "раз".len() });
    }

    #[test]
    fn styling_selection_splits_runs() {
        let mut ed = Editor::new();
        ed.insert_str("abcdef");
        ed.set_caret(Pos { block: 0, off: 1 }, false);
        ed.set_caret(Pos { block: 0, off: 4 }, true);
        ed.toggle_style(StyleBit::Bold);
        let Block::Para(runs) = &ed.doc().blocks[0] else { panic!() };
        assert_eq!(runs.len(), 3);
        assert_eq!(runs[1].text, "bcd");
        assert!(runs[1].style.bold);
        assert!(!runs[0].style.bold && !runs[2].style.bold);
        assert_eq!(ed.html(), "<div>a<b>bcd</b>ef</div>");
    }

    #[test]
    fn toggle_style_twice_clears_and_merges() {
        let mut ed = Editor::new();
        ed.insert_str("abcdef");
        ed.select_all();
        ed.toggle_style(StyleBit::Italic);
        ed.toggle_style(StyleBit::Italic);
        let Block::Para(runs) = &ed.doc().blocks[0] else { panic!() };
        assert_eq!(runs.len(), 1, "прогоны одного формата должны склеиться");
        assert_eq!(ed.html(), "<div>abcdef</div>");
    }

    #[test]
    fn pending_style_applies_to_next_input() {
        let mut ed = Editor::new();
        ed.insert_str("a");
        ed.toggle_style(StyleBit::Bold);
        ed.insert_str("b");
        assert_eq!(ed.html(), "<div>a<b>b</b></div>");
    }

    #[test]
    fn selection_across_blocks_deletes_and_joins() {
        let mut ed = Editor::new();
        ed.insert_str("раз\nдва\nтри");
        // Смещения байтовые: 2 байта — ровно одна кириллическая буква.
        ed.set_caret(Pos { block: 0, off: 2 }, false);
        ed.set_caret(Pos { block: 2, off: 2 }, true);
        ed.delete_selection();
        assert_eq!(ed.plain_text(), "рри");
    }

    #[test]
    fn image_sits_in_own_block_and_survives_html() {
        let mut ed = Editor::new();
        ed.insert_str("до");
        ed.insert_image(img());
        ed.insert_str("после");
        assert_eq!(ed.images().len(), 1);
        assert_eq!(
            ed.html(),
            "<div>до</div><div><img src=\"cid:img1@ddmail\" style=\"max-width:100%\"></div><div>после</div>"
        );
        assert_eq!(ed.plain_text(), "до\n\nпосле");
    }

    #[test]
    fn backspace_removes_image() {
        let mut ed = Editor::new();
        ed.insert_image(img());
        assert!(!ed.is_empty());
        ed.backspace();
        assert!(ed.is_empty());
        assert_eq!(ed.images().len(), 0);
    }

    #[test]
    fn undo_redo_round_trip() {
        let mut ed = Editor::new();
        ed.insert_str("раз");
        ed.insert_str(" два");
        ed.undo();
        assert_eq!(ed.plain_text(), "раз");
        ed.redo();
        assert_eq!(ed.plain_text(), "раз два");
    }

    #[test]
    fn html_escapes_and_autolinks() {
        let mut ed = Editor::new();
        ed.insert_str("см. https://example.com/a?b=1&c=2, там <всё>");
        assert_eq!(
            ed.html(),
            "<div>см. <a href=\"https://example.com/a?b=1&amp;c=2\">https://example.com/a?b=1&amp;c=2</a>, там &lt;всё&gt;</div>"
        );
    }

    #[test]
    fn explicit_link_over_selection() {
        let mut ed = Editor::new();
        ed.insert_str("наш сайт");
        ed.select_all();
        ed.insert_link("https://example.com");
        assert_eq!(ed.html(), "<div><a href=\"https://example.com\">наш сайт</a></div>");
    }

    #[test]
    fn word_motion_and_selection_text() {
        let mut ed = Editor::new();
        ed.insert_str("one two three");
        ed.move_caret(Motion::DocStart, false);
        ed.move_caret(Motion::WordRight, true);
        assert_eq!(ed.selection_text(), "one");
    }

    #[test]
    fn select_word_at_click() {
        let mut ed = Editor::new();
        ed.insert_str("раз два три");
        ed.select_word_at(Pos { block: 0, off: "раз д".len() });
        assert_eq!(ed.selection_text(), "два");
    }

    #[test]
    fn clamp_never_splits_utf8() {
        let mut ed = Editor::new();
        ed.insert_str("ёж");
        // Смещение 1 — середина двухбайтовой «ё»: должно съехать на 0.
        ed.set_caret(Pos { block: 9, off: 1 }, false);
        assert_eq!(ed.caret(), Pos { block: 0, off: 0 });
    }

    #[test]
    fn empty_doc_is_empty_but_with_image_is_not() {
        let mut ed = Editor::new();
        assert!(ed.is_empty());
        ed.insert_image(img());
        assert!(!ed.is_empty());
        ed.clear();
        assert!(ed.is_empty());
    }
}
