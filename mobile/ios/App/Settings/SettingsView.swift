import DDMailAuth
import SwiftUI
import UserNotifications

/// Настройки (Android «Настройки», модалка десктопа): учётки, загрузка писем,
/// уведомления, календари. Открываются из бургера любого раздела.
struct SettingsView: View {
    @Bindable var settings = AppSettings.shared
    @Bindable var calPrefs = CalendarPrefs.shared
    let app: AppModel
    let workspace: Workspace
    var onRemindersChanged: () -> Void = {}
    @Environment(\.dismiss) private var dismiss
    @Environment(\.openURL) private var openURL
    @State private var denied = false

    var body: some View {
        NavigationStack {
            Form {
                AccountsSection(app: app, workspace: workspace, close: { dismiss() })
                if let mail = workspace.mail {
                    Section {
                        NavigationLink {
                            FoldersSettingsView(mail: mail, workspace: workspace)
                        } label: {
                            Label("Папки", systemImage: "folder")
                        }
                        .accessibilityIdentifier("settings.folders")
                    } footer: {
                        Text("Табы папок над списком бесед: показывать ли их на этом устройстве, правила, порядок.")
                    }
                }
                Section {
                    NavigationLink {
                        ExternalContentView()
                    } label: {
                        Label("Внешний контент", systemImage: "photo.badge.checkmark")
                    }
                    .accessibilityIdentifier("settings.external")
                } footer: {
                    Text("От кого картинки в письмах показываются сразу — посмотреть и отозвать.")
                }
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
                if let cal = workspace.calendar {
                    Section {
                        NavigationLink {
                            CalendarsListView(model: cal)
                        } label: {
                            Label("Календари", systemImage: "calendar")
                        }
                    } footer: {
                        Text("Какие календари видны на этом устройстве и в какой попадают новые события.")
                    }
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
        }
    }
}
