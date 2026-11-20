import DDMailAuth
import DDMailSync
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
                    DefaultSenderSection(mail: mail)
                }
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
                            #else
                            // «Системные настройки → Уведомления» на строке приложения (M-13).
                            if let u = URL(string: "x-apple.systempreferences:com.apple.Notifications-Settings.extension?id=\(Bundle.main.bundleIdentifier ?? "ru.letotam.ddmail")") {
                                openURL(u)
                            }
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

/// «Отправитель по умолчанию» (десктоп §1в, ★ во вкладке «Айдентики»): с
/// какой группы адресов уходит новое письмо и ответ на письмо, пришедшее не
/// на мой адрес. Строка — имя группы и её адрес-отправитель. Выбор пишется в
/// `default_group` общего документа групп: с ddmail — на сервер (виден
/// десктопу и другим устройствам), без него — на этом устройстве.
struct DefaultSenderSection: View {
    let mail: MailModel
    @State private var error: String?

    var body: some View {
        let groups = mail.groups.groups.filter { !$0.sender.isEmpty }
        let current = mail.groups.defaultGroup?.id ?? ""
        Section {
            if groups.isEmpty {
                Text("Своих адресов пока нет — они появятся после синхронизации учёток.")
                    .foregroundStyle(.secondary)
            } else {
                Picker("Отправитель", selection: Binding(get: { current }, set: { pick($0, in: groups) })) {
                    ForEach(groups, id: \.id) { g in
                        VStack(alignment: .leading) {
                            Text(g.name.trimmingCharacters(in: .whitespaces).isEmpty ? g.sender : g.name)
                            if !g.name.trimmingCharacters(in: .whitespaces).isEmpty {
                                Text(g.sender).font(.caption).foregroundStyle(.secondary)
                            }
                        }
                        .tag(g.id)
                    }
                }
                .pickerStyle(.inline)
                .labelsHidden()
                .accessibilityIdentifier("settings.defaultSender")
            }
            if let error {
                Label(error, systemImage: "exclamationmark.triangle").foregroundStyle(.red)
            }
        } header: {
            Text("Отправитель по умолчанию")
        } footer: {
            Text("Новое письмо и ответ на письмо, пришедшее не на ваш адрес, уходят с этого адреса. "
                + (mail.groupsShared ? "Настройка общая с десктопом и другими устройствами." : "Настройка хранится на этом устройстве."))
        }
    }

    private func pick(_ id: String, in groups: [IdentityGroups.Group]) {
        guard id != mail.groups.defaultGroup?.id, let g = groups.first(where: { $0.id == id }) else { return }
        error = nil
        Task {
            do { try await mail.setDefaultSender(g.sender) } catch {
                self.error = error is IdentityGroupsBusy || error is IdentityGroupGone
                    ? error.localizedDescription : "Не сохранено: \(error.localizedDescription)"
            }
        }
    }
}
