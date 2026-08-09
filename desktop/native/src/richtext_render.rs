//! Раскладка и растеризация rich-text композера: модель ([`crate::richtext`])
//! → битмап для Slint + геометрия каретки и хит-тесты.
//!
//! Почему битмап, а не элементы Slint: смешанный инлайн-стиль требует
//! пословной вёрстки с переносами, а у Slint нет ни flow-раскладки, ни
//! доступа к метрикам шрифта из Rust. Поэтому текст верстает и рисует
//! cosmic-text (тот же rustybuzz/swash, что под капотом у большинства
//! редакторов), а Slint показывает готовый `Image` и рисует поверх только
//! каретку. Тот же приём, что уже работает для тел писем (там растеризует
//! WebKit) — но здесь без раунд-трипа в веб-движок, чтобы ввод не тормозил.
//!
//! Единицы: наружу всё в ЛОГИЧЕСКИХ px (координаты Slint), внутри вёрстка
//! идёт в ФИЗИЧЕСКИХ (логические × scale factor) — иначе на HiDPI текст
//! мылит.

use std::collections::HashMap;

use cosmic_text::{
    Attrs, Buffer, Color, Cursor, Family, FontSystem, Metrics, Shaping, Style as FontStyle,
    SwashCache, UnderlineStyle, Weight,
};
use slint::{Image, Rgba8Pixel, SharedPixelBuffer};

use crate::richtext::{Block, Doc, Pos};

/// Кегль и интерлиньяж композера (логические px) — как у прежнего TextInput.
pub const FONT_PX: f32 = 15.0;
pub const LINE_PX: f32 = 21.0;
/// Вертикальный зазор между блоками.
const BLOCK_GAP: f32 = 2.0;
/// Картинка не растягивается шире колонки и не выше этого (логические px) —
/// иначе одна вставка выжирает весь композер.
const MAX_IMG_H: f32 = 220.0;

/// Фон пузыря композера (#f0f2f5) — рисуем непрозрачным, чтобы сглаживание
/// глифов ложилось на тот же цвет, что и вокруг битмапа.
const BG: [u8; 3] = [0xf0, 0xf2, 0xf5];
const FG: Color = Color::rgb(0x0f, 0x14, 0x19);
/// Ссылки — синим и подчёркнутыми, как их отрисует почтовик получателя.
const LINK: Color = Color::rgb(0x1d, 0x6f, 0xd6);
/// Выделение — фирменный зелёный композера с прозрачностью.
const SEL: [u8; 4] = [0x10, 0xb9, 0x81, 0x66];

pub struct Rendered {
    pub image: Image,
    /// Полная высота содержимого, логические px.
    pub height: f32,
    pub caret_x: f32,
    pub caret_y: f32,
    pub caret_h: f32,
}

/// Геометрия одного блока последней раскладки (логические px).
enum Laid {
    Para { buf: Buffer, top: f32, height: f32 },
    Image { top: f32, height: f32, width: f32 },
}

impl Laid {
    fn top(&self) -> f32 {
        match self {
            Laid::Para { top, .. } | Laid::Image { top, .. } => *top,
        }
    }
    fn height(&self) -> f32 {
        match self {
            Laid::Para { height, .. } | Laid::Image { height, .. } => *height,
        }
    }
}

pub struct Renderer {
    fs: FontSystem,
    swash: SwashCache,
    /// Семейство, найденное в системе при старте (см. [`pick_family`]).
    family: String,
    /// Раскладка последнего кадра — по ней отвечают хит-тесты и вертикальные
    /// шаги каретки. Всегда свежая: любая правка перерисовывает композер.
    laid: Vec<Laid>,
    /// Декодированные картинки (исходный размер) и уже отмасштабированные под
    /// текущую ширину — декодировать PNG на каждое нажатие клавиши нельзя.
    decoded: HashMap<String, image::RgbaImage>,
    scaled: HashMap<(String, u32, u32), image::RgbaImage>,
    width: f32,
    scale: f32,
}

impl Renderer {
    /// Сборка шрифтовой системы читает системные шрифты (сотни миллисекунд) —
    /// поэтому создаётся один раз и живёт вместе с окном.
    pub fn new() -> Self {
        let mut fs = FontSystem::new();
        let family = pick_family(&mut fs);
        Renderer {
            fs,
            swash: SwashCache::new(),
            family,
            laid: Vec::new(),
            decoded: HashMap::new(),
            scaled: HashMap::new(),
            width: 0.0,
            scale: 1.0,
        }
    }

    /// Разложить документ и нарисовать кадр.
    ///
    /// `width` — ширина колонки в логических px, `scale` — scale factor окна,
    /// `sel` — нормализованное выделение (начало ≤ конец) либо `None`.
    pub fn render(
        &mut self,
        doc: &Doc,
        caret: Pos,
        sel: Option<(Pos, Pos)>,
        width: f32,
        scale: f32,
    ) -> Rendered {
        let width = width.max(16.0);
        let scale = if scale.is_finite() && scale > 0.0 { scale } else { 1.0 };
        self.width = width;
        self.scale = scale;
        self.layout(doc, width, scale);

        let total_h = self
            .laid
            .last()
            .map(|b| b.top() + b.height())
            .unwrap_or(LINE_PX)
            .max(LINE_PX);
        let px_w = (width * scale).round().max(1.0) as u32;
        let px_h = (total_h * scale).ceil().max(1.0) as u32;

        let mut canvas = SharedPixelBuffer::<Rgba8Pixel>::new(px_w, px_h);
        {
            let stride = px_w as usize;
            let pixels = canvas.make_mut_slice();
            for p in pixels.iter_mut() {
                *p = Rgba8Pixel { r: BG[0], g: BG[1], b: BG[2], a: 255 };
            }

            // Порядок: подложка выделения → текст (глифы и подчёркивания) →
            // картинки. Текст поверх выделения, иначе буквы окажутся под ним.
            let mut laid = std::mem::take(&mut self.laid);
            for (bi, block) in laid.iter().enumerate() {
                let top_px = (block.top() * scale).round() as i32;
                match block {
                    Laid::Para { buf, .. } => {
                        if let Some((s, e)) = sel {
                            if bi >= s.block && bi <= e.block {
                                let start = if bi == s.block { s.off } else { 0 };
                                let end = if bi == e.block { e.off } else { usize::MAX };
                                for run in buf.layout_runs() {
                                    for (x, w) in run
                                        .highlight(Cursor::new(0, start), Cursor::new(0, end))
                                    {
                                        fill_rect(
                                            pixels,
                                            stride,
                                            px_h,
                                            x.round() as i32,
                                            top_px + run.line_top.round() as i32,
                                            w.ceil() as i32,
                                            run.line_height.ceil() as i32,
                                            SEL,
                                        );
                                    }
                                }
                                // Пустой абзац внутри выделения: показываем
                                // «захвачен перевод строки» узкой полоской,
                                // иначе выделение через пустую строку рвётся.
                                if buf.layout_runs().all(|r| r.glyphs.is_empty())
                                    && (bi > s.block || start == 0)
                                    && bi < e.block
                                {
                                    fill_rect(
                                        pixels,
                                        stride,
                                        px_h,
                                        0,
                                        top_px,
                                        (6.0 * scale) as i32,
                                        (LINE_PX * scale) as i32,
                                        SEL,
                                    );
                                }
                            }
                        }
                    }
                    Laid::Image { .. } => {
                        if let Some((s, e)) = sel {
                            if bi >= s.block && bi <= e.block && (bi > s.block || s.off == 0) {
                                fill_rect(
                                    pixels,
                                    stride,
                                    px_h,
                                    0,
                                    top_px,
                                    px_w as i32,
                                    (block.height() * scale) as i32,
                                    SEL,
                                );
                            }
                        }
                    }
                }
            }
            // iter_mut, а не клон буфера: `Buffer::draw` требует &mut (он
            // дошейпливает видимую часть), а клонировать разложенный абзац на
            // каждое нажатие клавиши — ровно та трата, ради которой мы ушли от
            // раунд-трипов в веб-движок.
            for block in laid.iter_mut() {
                if let Laid::Para { buf, top, .. } = block {
                    let top_px = (*top * scale).round() as i32;
                    buf.draw(&mut self.fs, &mut self.swash, FG, |x, y, w, h, color| {
                        let a = color.a();
                        if a == 0 {
                            return;
                        }
                        fill_rect(
                            pixels,
                            stride,
                            px_h,
                            x,
                            y + top_px,
                            w as i32,
                            h as i32,
                            [color.r(), color.g(), color.b(), a],
                        );
                    });
                }
            }
            self.laid = laid;

            // Картинки рисуем последними, поверх подложки выделения.
            for (bi, block) in self.laid.iter().enumerate() {
                if let (Laid::Image { top, height, width: bw }, Some(Block::Image(img))) =
                    (block, doc.blocks.get(bi))
                {
                    let w_px = (*bw * scale).round().max(1.0) as u32;
                    let h_px = ((*height - BLOCK_GAP).max(1.0) * scale).round().max(1.0) as u32;
                    let key = (img.cid.clone(), w_px, h_px);
                    if !self.scaled.contains_key(&key) {
                        if let Some(src) = self.decoded.get(&img.cid) {
                            let resized = image::imageops::resize(
                                src,
                                w_px,
                                h_px,
                                image::imageops::FilterType::Triangle,
                            );
                            self.scaled.insert(key.clone(), resized);
                        }
                    }
                    if let Some(bitmap) = self.scaled.get(&key) {
                        blit(pixels, stride, px_h, bitmap, 0, (*top * scale).round() as i32);
                    }
                }
            }
        }

        let (cx, cy, ch) = self.caret_rect(caret);
        Rendered {
            image: Image::from_rgba8(canvas),
            height: total_h,
            caret_x: cx,
            caret_y: cy,
            caret_h: ch,
        }
    }

    /// Позиция в документе по точке (логические px) — клик и протяжка мышью.
    pub fn pos_at(&self, x: f32, y: f32) -> Pos {
        if self.laid.is_empty() {
            return Pos::default();
        }
        let mut idx = self.laid.len() - 1;
        for (i, b) in self.laid.iter().enumerate() {
            if y < b.top() + b.height() {
                idx = i;
                break;
            }
        }
        match &self.laid[idx] {
            Laid::Image { .. } => Pos { block: idx, off: 0 },
            Laid::Para { buf, top, .. } => {
                let local_y = ((y - top) * self.scale).max(0.0);
                let local_x = (x * self.scale).max(0.0);
                let off = buf.hit(local_x, local_y).map(|c| c.index).unwrap_or(0);
                Pos { block: idx, off }
            }
        }
    }

    /// Шаг каретки вверх/вниз по ВИЗУАЛЬНЫМ строкам (внутри абзаца перенос —
    /// тоже строка), с переходом между блоками. Модель вертикали не знает:
    /// она зависит от переносов, а те живут здесь.
    pub fn move_vertical(&self, pos: Pos, up: bool) -> Pos {
        let (x, y, h) = self.caret_rect(pos);
        let probe_y = if up { y - h * 0.5 } else { y + h * 1.5 };
        if probe_y < 0.0 {
            return Pos { block: 0, off: 0 };
        }
        self.pos_at(x, probe_y)
    }

    /// Прямоугольник каретки (логические px): x, y верхнего края, высота.
    pub fn caret_rect(&self, pos: Pos) -> (f32, f32, f32) {
        let Some(block) = self.laid.get(pos.block) else {
            return (0.0, 0.0, LINE_PX);
        };
        match block {
            Laid::Image { top, height, .. } => (0.0, *top, (*height - BLOCK_GAP).max(LINE_PX)),
            Laid::Para { buf, top, .. } => {
                let cursor = Cursor::new(0, pos.off);
                match buf.cursor_position(&cursor) {
                    Some((x, line_top)) => (
                        x / self.scale,
                        top + line_top / self.scale,
                        LINE_PX,
                    ),
                    None => (0.0, *top, LINE_PX),
                }
            }
        }
    }

    /// Забыть растровые кэши картинки, которой больше нет в документе
    /// (например, откатили вставку) — иначе память растёт до перезапуска.
    pub fn forget_unused(&mut self, doc: &Doc) {
        let live: Vec<String> = doc
            .blocks
            .iter()
            .filter_map(|b| match b {
                Block::Image(img) => Some(img.cid.clone()),
                Block::Para(_) => None,
            })
            .collect();
        self.decoded.retain(|cid, _| live.contains(cid));
        self.scaled.retain(|(cid, _, _), _| live.contains(cid));
    }

    // ── Внутреннее ─────────────────────────────────────────────────────────

    fn layout(&mut self, doc: &Doc, width: f32, scale: f32) {
        let metrics = Metrics::new(FONT_PX * scale, LINE_PX * scale);
        // Семейство копируем: `Attrs` держит на него ссылку, а декодирование
        // картинок ниже берёт `self` целиком по &mut.
        let family = self.family.clone();
        let base = Attrs::new().family(Family::Name(&family)).color(FG);
        let mut laid: Vec<Laid> = Vec::with_capacity(doc.blocks.len());
        let mut y = 0.0f32;
        for block in &doc.blocks {
            match block {
                Block::Para(runs) => {
                    let mut buf = Buffer::new(&mut self.fs, metrics);
                    buf.set_size(Some(width * scale), None);
                    if runs.iter().all(|r| r.text.is_empty()) {
                        // Пустой абзац всё равно занимает строку — каретке
                        // нужно где-то стоять.
                        buf.set_text("", &base, Shaping::Advanced, None);
                    } else {
                        let spans: Vec<(&str, Attrs)> = runs
                            .iter()
                            .filter(|r| !r.text.is_empty())
                            .map(|r| {
                                let mut a = base.clone();
                                if r.style.bold {
                                    a = a.weight(Weight::BOLD);
                                }
                                if r.style.italic {
                                    a = a.style(FontStyle::Italic);
                                }
                                if r.style.underline || r.link.is_some() {
                                    a = a.underline(UnderlineStyle::Single);
                                }
                                if r.link.is_some() {
                                    a = a.color(LINK).underline_color(LINK);
                                }
                                (r.text.as_str(), a)
                            })
                            .collect();
                        buf.set_rich_text(spans, &base, Shaping::Advanced, None);
                    }
                    buf.shape_until_scroll(&mut self.fs, false);
                    let h = buf
                        .layout_runs()
                        .map(|r| r.line_top + r.line_height)
                        .fold(0.0f32, f32::max)
                        / scale;
                    let h = if h > 0.0 { h } else { LINE_PX };
                    laid.push(Laid::Para { buf, top: y, height: h + BLOCK_GAP });
                    y += h + BLOCK_GAP;
                }
                Block::Image(img) => {
                    self.decode(img);
                    let (nat_w, nat_h) = self
                        .decoded
                        .get(&img.cid)
                        .map(|i| (i.width() as f32, i.height() as f32))
                        .unwrap_or((img.w.max(1) as f32, img.h.max(1) as f32));
                    // Вписываем в колонку и в потолок высоты, не увеличивая
                    // мелкие картинки.
                    let k = (width / nat_w).min(MAX_IMG_H / nat_h).min(1.0);
                    let (w, h) = ((nat_w * k).max(1.0), (nat_h * k).max(1.0));
                    laid.push(Laid::Image { top: y, height: h + BLOCK_GAP, width: w });
                    y += h + BLOCK_GAP;
                }
            }
        }
        self.laid = laid;
    }

    fn decode(&mut self, img: &crate::richtext::InlineImage) {
        if self.decoded.contains_key(&img.cid) {
            return;
        }
        match image::load_from_memory(&img.bytes) {
            Ok(d) => {
                self.decoded.insert(img.cid.clone(), d.to_rgba8());
            }
            Err(e) => eprintln!("richtext: не смог декодировать вставленную картинку: {e}"),
        }
    }
}

/// Первое семейство из списка, которое реально есть в системе. Список — то,
/// чем набран остальной интерфейс: композер не должен выделяться шрифтом.
fn pick_family(fs: &mut FontSystem) -> String {
    const WANTED: [&str; 6] =
        ["Segoe UI", "Inter", "Noto Sans", "DejaVu Sans", "Liberation Sans", "Arial"];
    let available: Vec<String> = fs
        .db()
        .faces()
        .flat_map(|f| f.families.iter().map(|(name, _)| name.clone()).collect::<Vec<_>>())
        .collect();
    for want in WANTED {
        if available.iter().any(|f| f == want) {
            return want.to_string();
        }
    }
    available.first().cloned().unwrap_or_else(|| "sans-serif".to_string())
}

/// Залить прямоугольник с альфа-смешением. Координаты — физические px
/// относительно левого верхнего угла битмапа; выход за края обрезается.
fn fill_rect(
    pixels: &mut [Rgba8Pixel],
    stride: usize,
    height: u32,
    x: i32,
    y: i32,
    w: i32,
    h: i32,
    rgba: [u8; 4],
) {
    if w <= 0 || h <= 0 || rgba[3] == 0 {
        return;
    }
    let x0 = x.max(0) as usize;
    let y0 = y.max(0) as usize;
    let x1 = ((x + w).max(0) as usize).min(stride);
    let y1 = ((y + h).max(0) as usize).min(height as usize);
    let a = rgba[3] as u32;
    for row in y0..y1 {
        for col in x0..x1 {
            let p = &mut pixels[row * stride + col];
            p.r = blend(p.r, rgba[0], a);
            p.g = blend(p.g, rgba[1], a);
            p.b = blend(p.b, rgba[2], a);
            p.a = 255;
        }
    }
}

fn blend(dst: u8, src: u8, a: u32) -> u8 {
    ((src as u32 * a + dst as u32 * (255 - a)) / 255) as u8
}

/// Наложить готовый RGBA-битмап (картинку) на холст.
fn blit(
    pixels: &mut [Rgba8Pixel],
    stride: usize,
    height: u32,
    src: &image::RgbaImage,
    x: i32,
    y: i32,
) {
    for (sx, sy, px) in src.enumerate_pixels() {
        let dx = x + sx as i32;
        let dy = y + sy as i32;
        if dx < 0 || dy < 0 || dx as usize >= stride || dy as usize >= height as usize {
            continue;
        }
        let a = px.0[3] as u32;
        if a == 0 {
            continue;
        }
        let p = &mut pixels[dy as usize * stride + dx as usize];
        p.r = blend(p.r, px.0[0], a);
        p.g = blend(p.g, px.0[1], a);
        p.b = blend(p.b, px.0[2], a);
        p.a = 255;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::richtext::Editor;

    /// Раскладка живого документа: высота растёт с числом строк, каретка
    /// уезжает вправо по мере набора, хит-тест возвращается в ту же позицию.
    /// Тест гоняет весь путь модель → cosmic-text → битмап, без окна.
    #[test]
    fn layout_geometry_is_sane() {
        let mut r = Renderer::new();
        let mut ed = Editor::new();
        ed.insert_str("привет");
        let one = r.render(ed.doc(), ed.caret(), None, 300.0, 1.0);
        assert!(one.height >= LINE_PX, "строка не может быть ниже интерлиньяжа");
        assert!(one.caret_x > 0.0, "каретка после текста должна съехать вправо");

        ed.split_block();
        ed.insert_str("вторая строка");
        let two = r.render(ed.doc(), ed.caret(), None, 300.0, 1.0);
        assert!(two.height > one.height, "второй абзац должен добавить высоту");
        assert!(two.caret_y > one.caret_y, "каретка должна уйти на строку ниже");

        // Клик в точку каретки возвращает ту же позицию в документе.
        let hit = r.pos_at(two.caret_x, two.caret_y + 2.0);
        assert_eq!(hit, ed.caret());
    }

    /// Вертикальный шаг идёт по визуальным строкам: вверх со второго абзаца —
    /// в первый, вниз возвращает обратно.
    #[test]
    fn vertical_motion_crosses_blocks() {
        let mut r = Renderer::new();
        let mut ed = Editor::new();
        ed.insert_str("первая\nвторая");
        r.render(ed.doc(), ed.caret(), None, 300.0, 1.0);
        let up = r.move_vertical(ed.caret(), true);
        assert_eq!(up.block, 0);
        let down = r.move_vertical(up, false);
        assert_eq!(down.block, 1);
    }

    /// Длинная строка переносится: при узкой колонке высота больше одной
    /// строки, хотя абзац один.
    #[test]
    fn long_line_wraps() {
        let mut r = Renderer::new();
        let mut ed = Editor::new();
        ed.insert_str("слово слово слово слово слово слово слово слово слово");
        let narrow = r.render(ed.doc(), ed.caret(), None, 120.0, 1.0);
        assert!(narrow.height > LINE_PX * 2.0, "узкая колонка должна дать переносы");
        let wide = r.render(ed.doc(), ed.caret(), None, 900.0, 1.0);
        assert!(wide.height < narrow.height);
    }
}
