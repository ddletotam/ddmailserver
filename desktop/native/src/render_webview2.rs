//! Windows email-body renderer backed by WebView2 (Edge/Chromium).
//!
//! Mirrors the WebKitGTK backend's API (`Engine` / `RenderResult` / `Bitmap`)
//! so main.rs is identical across platforms. We host a single WebView2 in a
//! hidden off-screen window, drive NavigateToString → measure scrollHeight via
//! ExecuteScript → resize → CapturePreview(PNG) → decode to RGBA.
//!
//! Threading: WebView2 is single-threaded and needs a message pump; everything
//! here runs on the render worker thread (main.rs spawns it and calls
//! Engine::new there). `wait_with_pump` pumps the Win32 loop until each async
//! COM op completes. Hit-testing is disabled (parity with the GTK migration).

use std::sync::mpsc;

use webview2_com::{
    CapturePreviewCompletedHandler, CreateCoreWebView2ControllerCompletedHandler,
    CreateCoreWebView2EnvironmentCompletedHandler, ExecuteScriptCompletedHandler,
    NavigationCompletedEventHandler, wait_with_pump,
};
use webview2_com::Microsoft::Web::WebView2::Win32::{
    CreateCoreWebView2EnvironmentWithOptions, ICoreWebView2, ICoreWebView2Controller,
    ICoreWebView2Environment, COREWEBVIEW2_CAPTURE_PREVIEW_IMAGE_FORMAT_PNG,
};
use windows::core::PCWSTR;
use windows::Win32::Foundation::{BOOL, E_POINTER, HWND, LPARAM, LRESULT, RECT, WPARAM};
use windows::Win32::System::Com::{CoInitializeEx, IStream, COINIT_APARTMENTTHREADED, STREAM_SEEK};
use windows::Win32::System::Com::StructuredStorage::CreateStreamOnHGlobal;
use windows::Win32::System::LibraryLoader::GetModuleHandleW;
use windows::Win32::UI::WindowsAndMessaging::{
    CreateWindowExW, DefWindowProcW, RegisterClassW, CW_USEDEFAULT, HMENU, WINDOW_EX_STYLE,
    WNDCLASSW, WS_OVERLAPPEDWINDOW,
};

use crate::render_common::{
    parse_link_rects, parse_text_runs, LinkRect, TextRun, LINK_RECTS_JS, TEXT_RUNS_JS,
};

/// Plain pass-through window procedure (the off-screen host needs no logic).
unsafe extern "system" fn wndproc(hwnd: HWND, msg: u32, wp: WPARAM, lp: LPARAM) -> LRESULT {
    DefWindowProcW(hwnd, msg, wp, lp)
}

const MAX_H: u32 = 6000;
/// Cap on waiting for the `load` event (NavigationCompleted). `load` waits for
/// every subresource, so a single slow/hung external image would otherwise
/// stall the whole render for the full timeout. Kept short; on expiry we fall
/// back to rendering the already-parsed DOM (see navigate_sync).
const LOAD_TIMEOUT_MS: u128 = 2500;

pub struct Bitmap {
    pub rgba: Vec<u8>,
    pub width: u32,
    pub height: u32,
}

pub struct RenderResult {
    pub bitmap: Bitmap,
    pub view_ready: bool,
    pub painted_height: u32,
    pub links: Vec<LinkRect>,
    /// Per-word text layer for mouse selection (PDF-viewer style).
    pub runs: Vec<TextRun>,
}

impl RenderResult {
    pub fn successful(&self) -> bool {
        self.view_ready && self.painted_height > 0
    }
}

pub struct Engine {
    _hwnd: HWND,
    _env: ICoreWebView2Environment,
    controller: ICoreWebView2Controller,
    webview: ICoreWebView2,
}

fn wide(s: &str) -> Vec<u16> {
    s.encode_utf16().chain(std::iter::once(0)).collect()
}

impl Engine {
    pub fn new() -> Self {
        unsafe {
            let _ = CoInitializeEx(None, COINIT_APARTMENTTHREADED);

            // Hidden off-screen host window (WebView2 needs a real HWND parent).
            let hinstance = GetModuleHandleW(None).expect("module handle");
            let class_name = wide("ddmail_wv2_host");
            let wc = WNDCLASSW {
                lpfnWndProc: Some(wndproc),
                hInstance: hinstance.into(),
                lpszClassName: PCWSTR(class_name.as_ptr()),
                ..Default::default()
            };
            RegisterClassW(&wc); // ignore "already registered" on repeat

            let hwnd = CreateWindowExW(
                WINDOW_EX_STYLE(0),
                PCWSTR(class_name.as_ptr()),
                PCWSTR(wide("ddmail").as_ptr()),
                WS_OVERLAPPEDWINDOW,
                CW_USEDEFAULT,
                CW_USEDEFAULT,
                900,
                900,
                None,
                HMENU::default(),
                hinstance,
                None,
            )
            .expect("create host window");

            // ----- Create environment (async) -----
            let (tx, rx) = mpsc::channel::<windows::core::Result<ICoreWebView2Environment>>();
            CreateCoreWebView2EnvironmentWithOptions(
                PCWSTR::null(),
                PCWSTR::null(),
                None,
                &CreateCoreWebView2EnvironmentCompletedHandler::create(Box::new(
                    move |error_code, environment| {
                        error_code?;
                        let _ = tx.send(environment.ok_or_else(|| {
                            windows::core::Error::from(E_POINTER)
                        }));
                        Ok(())
                    },
                )),
            )
            .expect("CreateCoreWebView2EnvironmentWithOptions");
            let env = wait_with_pump(rx)
                .expect("env pump")
                .expect("env result");

            // ----- Create controller (async) -----
            let (tx, rx) = mpsc::channel::<windows::core::Result<ICoreWebView2Controller>>();
            env.CreateCoreWebView2Controller(
                hwnd,
                &CreateCoreWebView2ControllerCompletedHandler::create(Box::new(
                    move |error_code, controller| {
                        error_code?;
                        let _ = tx.send(controller.ok_or_else(|| {
                            windows::core::Error::from(E_POINTER)
                        }));
                        Ok(())
                    },
                )),
            )
            .expect("CreateCoreWebView2Controller");
            let controller = wait_with_pump(rx).expect("ctl pump").expect("ctl result");

            let _ = controller.SetIsVisible(true);
            let webview = controller.CoreWebView2().expect("CoreWebView2");

            Engine {
                _hwnd: hwnd,
                _env: env,
                controller,
                webview,
            }
        }
    }

    pub fn render_one(&mut self, html: &str, width: u32) -> RenderResult {
        unsafe {
            let w = width.max(1) as i32;
            // Small viewport first so scrollHeight reflects content, not the box.
            let _ = self.controller.SetBounds(RECT { left: 0, top: 0, right: w, bottom: 1 });

            let view_ready = self.navigate_sync(html);
            if !view_ready {
                return RenderResult {
                    bitmap: empty_bitmap(width),
                    view_ready: false,
                    painted_height: 0,
                    links: Vec::new(),
                    runs: Vec::new(),
                };
            }

            let painted_height = self.measure_height().min(MAX_H);
            if painted_height == 0 {
                return RenderResult {
                    bitmap: empty_bitmap(width),
                    view_ready: true,
                    painted_height: 0,
                    links: Vec::new(),
                    runs: Vec::new(),
                };
            }

            let links = self.extract_links();
            let runs = self.extract_text_runs();

            // Grow viewport to fit content so CapturePreview covers it all.
            let _ = self.controller.SetBounds(RECT {
                left: 0,
                top: 0,
                right: w,
                bottom: painted_height as i32,
            });
            pump_a_bit();

            let bitmap = self
                .capture(width, painted_height)
                .unwrap_or_else(|| empty_bitmap(width));

            RenderResult {
                bitmap,
                view_ready,
                painted_height,
                links,
                runs,
            }
        }
    }

    /// Run a JS expression and wait (bounded) for its JSON-encoded result.
    unsafe fn eval_json(&self, js: &str) -> Option<String> {
        let (tx, rx) = mpsc::channel::<String>();
        let script = wide(js);
        let _ = self.webview.ExecuteScript(
            PCWSTR(script.as_ptr()),
            &ExecuteScriptCompletedHandler::create(Box::new(move |_hr, json| {
                let _ = tx.send(json);
                Ok(())
            })),
        );
        let start = std::time::Instant::now();
        loop {
            if let Ok(s) = rx.try_recv() {
                return Some(s);
            }
            if start.elapsed().as_millis() > 2000 {
                return None;
            }
            pump_a_bit();
        }
    }

    /// Extract `<a href>` rects via JS (document-relative CSS px), JSON.
    unsafe fn extract_links(&self) -> Vec<LinkRect> {
        // ExecuteScript JSON-encodes its result; LINK_RECTS_JS evaluates to an
        // array, so the payload is already the array JSON.
        self.eval_json(LINK_RECTS_JS)
            .map(|raw| parse_link_rects(&raw))
            .unwrap_or_default()
    }

    /// Extract the per-word text layer (selection support).
    unsafe fn extract_text_runs(&self) -> Vec<TextRun> {
        self.eval_json(TEXT_RUNS_JS)
            .map(|raw| parse_text_runs(&raw))
            .unwrap_or_default()
    }

    pub fn clear_views(&mut self) {}

    /// Hit-testing disabled in this backend (parity with the GTK migration).
    pub fn hit(&self, _row: usize, _x: f32, _y: f32) -> Option<String> {
        None
    }

    unsafe fn navigate_sync(&self, html: &str) -> bool {
        let (tx, rx) = mpsc::channel::<bool>();
        let mut token = Default::default();
        let _ = self.webview.add_NavigationCompleted(
            &NavigationCompletedEventHandler::create(Box::new(move |_wv, args| {
                let ok = args
                    .map(|a| {
                        let mut s = BOOL(0);
                        let _ = a.IsSuccess(&mut s);
                        s.as_bool()
                    })
                    .unwrap_or(false);
                let _ = tx.send(ok);
                Ok(())
            })),
            &mut token,
        );

        let html_w = wide(html);
        if self.webview.NavigateToString(PCWSTR(html_w.as_ptr())).is_err() {
            return false;
        }

        // Pump until the navigation completes (or times out).
        let start = std::time::Instant::now();
        loop {
            if let Ok(ok) = rx.try_recv() {
                return ok;
            }
            if start.elapsed().as_millis() > LOAD_TIMEOUT_MS {
                // `load` never fired — almost always a slow/hung external
                // image (media allowed). If the DOM itself parsed, render what
                // we have (text + whatever images already arrived) rather than
                // returning a blank bubble.
                return self.dom_parsed();
            }
            pump_a_bit();
        }
    }

    /// True once the document has finished parsing (DOMContentLoaded), even if
    /// subresources are still loading. Lets us snapshot text-heavy mail whose
    /// external images never finish.
    unsafe fn dom_parsed(&self) -> bool {
        match self.eval_json("document.readyState") {
            Some(s) => s.contains("interactive") || s.contains("complete"),
            None => false,
        }
    }

    unsafe fn measure_height(&self) -> u32 {
        let (tx, rx) = mpsc::channel::<u32>();
        let script = wide(
            "Math.max(document.documentElement?document.documentElement.scrollHeight:0,\
             document.body?document.body.scrollHeight:0)",
        );
        let _ = self.webview.ExecuteScript(
            PCWSTR(script.as_ptr()),
            &ExecuteScriptCompletedHandler::create(Box::new(move |_hr, json| {
                // ExecuteScript returns a JSON-encoded result (e.g. "1234").
                let n = json.trim().trim_matches('"').parse::<u32>().unwrap_or(0);
                let _ = tx.send(n);
                Ok(())
            })),
        );
        let start = std::time::Instant::now();
        loop {
            if let Ok(n) = rx.try_recv() {
                return n;
            }
            if start.elapsed().as_millis() > 2000 {
                return 0;
            }
            pump_a_bit();
        }
    }

    unsafe fn capture(&self, width: u32, height: u32) -> Option<Bitmap> {
        let stream: IStream = CreateStreamOnHGlobal(None, true).ok()?;

        let (tx, rx) = mpsc::channel::<bool>();
        self.webview
            .CapturePreview(
                COREWEBVIEW2_CAPTURE_PREVIEW_IMAGE_FORMAT_PNG,
                &stream,
                &CapturePreviewCompletedHandler::create(Box::new(move |hr| {
                    let _ = tx.send(hr.is_ok());
                    Ok(())
                })),
            )
            .ok()?;

        let start = std::time::Instant::now();
        let ok = loop {
            if let Ok(ok) = rx.try_recv() {
                break ok;
            }
            if start.elapsed().as_millis() > 4000 {
                break false;
            }
            pump_a_bit();
        };
        if !ok {
            return None;
        }

        // Read the PNG bytes back out of the stream.
        let png = read_stream(&stream)?;
        let img = image::load_from_memory(&png).ok()?;
        let rgba = img.to_rgba8();
        let (iw, ih) = (rgba.width(), rgba.height());
        // Crop to the measured content height; PNG width should already match.
        let h = ih.min(height.max(1)).min(MAX_H);
        let w = iw.min(width.max(1));
        let row = iw as usize * 4;
        let src = rgba.into_raw();
        let mut out = Vec::with_capacity(w as usize * h as usize * 4);
        for y in 0..h as usize {
            let start = y * row;
            out.extend_from_slice(&src[start..start + w as usize * 4]);
        }
        Some(Bitmap { rgba: out, width: w, height: h })
    }
}

/// Read an entire IStream into a byte vector (seek to start, read in chunks).
unsafe fn read_stream(stream: &IStream) -> Option<Vec<u8>> {
    // Seek to end to learn the size, then back to start.
    let mut new_pos = 0u64;
    stream.Seek(0, STREAM_SEEK(2), Some(&mut new_pos)).ok()?; // STREAM_SEEK_END
    let size = new_pos as usize;
    stream.Seek(0, STREAM_SEEK(0), None).ok()?; // STREAM_SEEK_SET
    let mut buf = vec![0u8; size];
    let mut read = 0u32;
    stream
        .Read(buf.as_mut_ptr() as *mut _, size as u32, Some(&mut read))
        .ok()
        .ok()?;
    buf.truncate(read as usize);
    Some(buf)
}

fn empty_bitmap(width: u32) -> Bitmap {
    // The buffer MUST match width×height×4. A single-pixel (4-byte) buffer for
    // a `width`-wide row is an inconsistent bitmap: SharedPixelBuffer and the
    // PNG encoder both assert on the length mismatch, and the panic takes out
    // the render worker — every later conversation then hangs on the loader.
    let w = width.max(1);
    Bitmap { rgba: vec![255u8; (w as usize) * 4], width: w, height: 1 }
}

/// Pump pending Win32 messages briefly so WebView2 can lay out / paint.
fn pump_a_bit() {
    use windows::Win32::UI::WindowsAndMessaging::{
        DispatchMessageW, PeekMessageW, TranslateMessage, MSG, PM_REMOVE,
    };
    unsafe {
        let mut msg = MSG::default();
        let mut n = 0;
        while PeekMessageW(&mut msg, None, 0, 0, PM_REMOVE).as_bool() && n < 50 {
            let _ = TranslateMessage(&msg);
            DispatchMessageW(&msg);
            n += 1;
        }
    }
    std::thread::sleep(std::time::Duration::from_millis(8));
}
