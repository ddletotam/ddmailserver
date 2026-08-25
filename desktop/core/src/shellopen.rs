//! Единственная точка, где URL или путь уходит системному обработчику.
//!
//! На Windows — `ShellExecuteW`, а НЕ `cmd /C start "" <url>`. `cmd` разбирает
//! командную строку до того, как её увидит браузер, и `&` для него —
//! разделитель команд: из `https://x/p?token=abc&utm_source=mail&id=42`
//! обработчику доставался `https://x/p?token=abc`, а остаток cmd пытался
//! выполнить («'utm_source' is not recognized…»). Хост и путь при этом живые,
//! поэтому сервер отвечал не «не открылось», а 4xx — ссылка-кнопка из письма
//! приходила без половины параметров. Тем же разбором портились `%VAR%`
//! (подстановка переменной окружения) и `^ | ( ) < >`. Плюс на каждый клик
//! мигало консольное окно.
//!
//! `ShellExecuteW` получает строку как есть, тем же путём, что двойной клик в
//! Explorer, и сразу возвращает код ошибки — не нужно ждать exit-код чужого
//! процесса, чтобы понять, что обработчика нет.

use std::path::Path;

/// Открыть URL в браузере (или другом обработчике схемы). Возврат — только
/// про запуск обработчика: 4xx от сервера сюда, разумеется, не доходит.
///
/// Схему НЕ проверяем: белый список — дело вызывающего, который знает
/// происхождение строки (см. `click_target` в клиенте: письмо — враждебный
/// ввод, и `file:`/`ms-*:` оттуда открывать нельзя).
pub fn open_url(url: &str) -> Result<(), String> {
    #[cfg(target_os = "windows")]
    return windows_impl::shell_execute(url);

    #[cfg(not(target_os = "windows"))]
    launch(std::ffi::OsStr::new(url), false)
}

/// Открыть файл его штатным приложением, не дожидаясь вердикта: вызов идёт из
/// async-контекста, а `xdg-open` умеет жить до закрытия LibreOffice — ждать
/// его значило бы занять рабочий поток рантайма на всё это время.
pub fn open_path(path: &Path) -> Result<(), String> {
    #[cfg(target_os = "windows")]
    return windows_impl::shell_execute(&path.to_string_lossy());

    #[cfg(not(target_os = "windows"))]
    launch(path.as_os_str(), false)
}

/// То же, но с вердиктом: провал открытия виден пользователю тостом, а не
/// только в stderr. На Unix ЖДЁТ exit-код обработчика — звать из своего
/// потока, не с UI-потока и не из рантайма.
pub fn open_path_checked(path: &Path) -> Result<(), String> {
    #[cfg(target_os = "windows")]
    return windows_impl::shell_execute(&path.to_string_lossy());

    #[cfg(not(target_os = "windows"))]
    launch(path.as_os_str(), true)
}

/// Unix-запуск: `xdg-open`/`open`, аргумент уходит процессу напрямую — никакой
/// шелл его не разбирает, так что `&` и `%` тут никогда и не ломались.
/// `wait` — дожидаться ли exit-кода (см. `open_path_checked`).
#[cfg(not(target_os = "windows"))]
fn launch(target: &std::ffi::OsStr, wait: bool) -> Result<(), String> {
    #[cfg(target_os = "macos")]
    let mut cmd = std::process::Command::new("open");
    #[cfg(not(target_os = "macos"))]
    let mut cmd = std::process::Command::new("xdg-open");
    cmd.arg(target);
    if !wait {
        // Без обработчика xdg-open сам показывает выбор приложения — это тот
        // диалог, которого пользователь и ждёт, когда ассоциации нет.
        return match cmd.spawn() {
            Ok(_) => Ok(()),
            #[cfg(target_os = "linux")]
            Err(_) => std::process::Command::new("gio")
                .arg("open")
                .arg(target)
                .spawn()
                .map(|_| ())
                .map_err(|e| format!("xdg-open и gio open не запустились: {e}")),
            #[cfg(not(target_os = "linux"))]
            Err(e) => Err(e.to_string()),
        };
    }
    match cmd.status() {
        Ok(st) if st.success() => Ok(()),
        Ok(st) => Err(format!("обработчик вернул код {}", st.code().unwrap_or(-1))),
        Err(e) => Err(e.to_string()),
    }
}

#[cfg(target_os = "windows")]
mod windows_impl {
    use std::os::windows::ffi::OsStrExt;
    use windows::core::PCWSTR;
    use windows::Win32::Foundation::HWND;
    use windows::Win32::System::Com::{CoInitializeEx, CoUninitialize, COINIT_APARTMENTTHREADED};
    use windows::Win32::UI::Shell::ShellExecuteW;
    use windows::Win32::UI::WindowsAndMessaging::SW_SHOWNORMAL;

    fn wide(s: &str) -> Vec<u16> {
        std::ffi::OsStr::new(s).encode_wide().chain(std::iter::once(0)).collect()
    }

    pub fn shell_execute(target: &str) -> Result<(), String> {
        let verb = wide("open");
        let file = wide(target);
        // ShellExecute может отдать вызов shell-расширению, а оно ждёт потока с
        // инициализированным COM. `RPC_E_CHANGED_MODE` — поток уже
        // инициализировали в другой модели: это нормально, и убирать за чужой
        // инициализацией нельзя, поэтому CoUninitialize только за своей.
        let ours = unsafe { CoInitializeEx(None, COINIT_APARTMENTTHREADED) }.is_ok();
        let inst = unsafe {
            ShellExecuteW(
                HWND::default(),
                PCWSTR(verb.as_ptr()),
                PCWSTR(file.as_ptr()),
                PCWSTR::null(),
                PCWSTR::null(),
                SW_SHOWNORMAL,
            )
        };
        if ours {
            unsafe { CoUninitialize() };
        }
        // Успех документирован как «значение больше 32»; всё остальное —
        // упакованный в хэндл код SE_ERR_*/Win32 (2 — обработчика нет,
        // 31 — нет ассоциации, 5 — доступ запрещён).
        let code = inst.0 as isize;
        if code > 32 {
            Ok(())
        } else {
            Err(format!("ShellExecuteW: код {code}"))
        }
    }
}
