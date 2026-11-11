import SwiftUI
import UserNotifications

/// Настройки (Android «Настройки»): загрузка писем, уведомления, учётная запись.
struct SettingsView: View {
    @Bindable var settings = AppSettings.shared
    @Bindable var calPrefs = CalendarPrefs.shared
    let accountKey: String
    let onLogout: () -> Void
    var onRemindersChanged: () -> Void = {}
    @Environment(\.dismiss) private var dismiss
    @Environment(\.openURL) private var openURL
    @State private var denied = false
    @State private var confirmLogout = false

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    Picker("Загружать письма заранее", selection: $settings.prefetch) {
                        Text("Не выбрано").tag(AppSettings.Prefetch?.none)
                        ForEach(AppSettings.Prefetch.allCases) { Text($0.title).tag(Optional($0)) }
                    }
                } header: {
                    Text("Загрузка писем")
                } footer: {
                    Text("Письма скачиваются на устройство до того, как беседу откроют, — тогда она открывается сразу и без сети. Список бесед обновляется при любом выборе.")
                }
                Section {
                    Toggle("Новые письма", isOn: $settings.notifications)
                    if denied {
                        Button("Разрешить в настройках системы", systemImage: "gear") {
                            #if os(iOS)
                            if let u = URL(string: UIApplication.openNotificationSettingsURLString) { openURL(u) }
                            #endif
                        }
                    }
                } header: {
                    Text("Уведомления")
                } footer: {
                    Text(denied
                         ? "Уведомления запрещены в настройках системы."
                         : "Текст уведомления собирается на устройстве: отправитель и тема через серверы Apple не передаются.")
                }
                Section {
                    Toggle("Напоминания о событиях", isOn: $calPrefs.reminders)
                        .onChange(of: calPrefs.reminders) { onRemindersChanged() }
                } footer: {
                    Text("Каскад напоминаний события; смахнули уведомление — по этому событию больше не напомнит.")
                }
                Section("Учётная запись") {
                    LabeledContent("Вход", value: accountKey)
                    Button("Выйти", systemImage: "rectangle.portrait.and.arrow.right", role: .destructive) { confirmLogout = true }
                }
            }
            .formStyle(.grouped)
            .navigationTitle("Настройки")
            .toolbarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Готово", systemImage: "checkmark") { dismiss() }
                }
            }
            .task {
                denied = await UNUserNotificationCenter.current().notificationSettings().authorizationStatus == .denied
            }
            .confirmationDialog("Выйти из учётной записи?", isPresented: $confirmLogout, titleVisibility: .visible) {
                Button("Выйти", role: .destructive) {
                    dismiss()
                    onLogout()
                }
            } message: {
                Text("Сессия и почта этой учётной записи будут удалены с устройства.")
            }
        }
    }
}
