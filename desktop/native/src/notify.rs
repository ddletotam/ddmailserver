//! Thin wrapper over `notify-rust` for desktop notifications. Cross-platform
//! (Linux zbus / Windows WinRT toast / macOS) behind one call; failures are
//! swallowed — a missing notification daemon must never crash the client.

/// Show a desktop notification. Best-effort.
pub fn notify(title: &str, body: &str) {
    if let Err(e) = notify_rust::Notification::new()
        .summary(title)
        .body(body)
        .show()
    {
        eprintln!("notify: {e}");
    }
}

/// То же, но по плашке можно кликнуть.
///
/// Действие с ключом `"default"` freedesktop-демоны вешают на само тело
/// уведомления (так предписывает спецификация), поэтому отдельной кнопки
/// пользователь не видит — он просто щёлкает по плашке. Без действия плашка
/// вообще ни на что не реагирует: именно так письма и открывались «никак».
///
/// Ждать нажатия приходится в отдельном потоке: `wait_for_action`
/// блокирующий, а звать его на UI-потоке значит заморозить клиент до
/// закрытия плашки. Поток живёт до действия или до закрытия по таймауту
/// (демон присылает `NotificationClosed`), после чего сам заканчивается.
#[cfg(not(windows))]
pub fn notify_clickable(title: &str, body: &str, on_click: impl FnOnce() + Send + 'static) {
    match notify_rust::Notification::new()
        .summary(title)
        .body(body)
        .action("default", "Открыть")
        .show()
    {
        Ok(handle) => {
            std::thread::spawn(move || {
                handle.wait_for_action(|action| {
                    if action == "default" {
                        on_click();
                    }
                });
            });
        }
        Err(e) => eprintln!("notify: {e}"),
    }
}
