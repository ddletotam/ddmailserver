import SwiftUI
import UserNotifications

/// Разделы приложения — как у десктопа (`docs/mobile-layout.md`): единственная
/// навигация между ними — бургер в первой строке каждого раздела, меню
/// «Почта, Календарь, Контакты, Задачи, Настройки». Ни таб-бара, ни боковой
/// колонки разделов. «Настройки» — окно поверх, как модалка десктопа.
struct AppShell: View {
    let app: AppModel
    let workspace: Workspace
    @SceneStorage("section") private var section: AppSection = .mail
    @State private var settingsShown = false
    @Environment(\.scenePhase) private var scenePhase

    var body: some View {
        // Учётки, которым нужен вход, — читаем в теле (контракт §0).
        let dead = workspace.dead.map { ($0.account.key, $0.account.title) }
        let current = section
        content(current)
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            // Акцент один на все разделы — зелёный десктопа.
            .tint(Palette.accent)
            .environment(\.sectionNav, SectionNav(current: current, go: { section = $0 }, settings: { settingsShown = true }))
            .focusedSceneValue(\.mailActions, mailActions)
            #if os(macOS)
            // Окно без заголовка и без кнопки боковой панели: над разделом ничего (mobile-layout.md).
            .toolbar(removing: .title)
            #endif
            .sheet(isPresented: $settingsShown) {
                SettingsView(app: app, workspace: workspace) {
                    Task { await workspace.reminders?.refresh() }
                }
                #if os(macOS)
                .frame(minWidth: 520, minHeight: 560)
                #endif
            }
            // «Нужен вход» — несмываемая плашка над содержимым у каждой такой
            // учётки, кэш виден (контракт §2, внешние учётки §7).
            .safeAreaInset(edge: .top, spacing: 0) {
                if !dead.isEmpty {
                    VStack(spacing: 0) {
                        ForEach(dead, id: \.0) { key, title in
                            SessionDeadBanner(title: dead.count > 1 || workspace.links.count > 1 ? title : nil) { app.relogin(key) }
                        }
                    }
                }
            }
            // Синк на выход приложения на экран (§4), живые каналы — пока на экране (§7).
            .task(id: scenePhase) {
                guard scenePhase == .active else {
                    // Ушли с экрана — каналы закрыты: в фоне их держать нельзя (§7).
                    workspace.mail?.live.forEach { $0.stop() }
                    return
                }
                guard let mail = workspace.mail else { return }
                NotificationRouter.shared.attach { [weak mail] ref in
                    // Открыть беседу из уведомления: раздел «Почта», её папка.
                    section = .mail
                    mail?.open(ref)
                }
                if let reminders = workspace.reminders {
                    NotificationRouter.shared.attachReminders { [workspace] key, action in
                        switch action {
                        case UNNotificationDismissActionIdentifier:
                            Task { await reminders.dismiss(key) }
                        case UNNotificationDefaultActionIdentifier:
                            // Тап — событие в календаре; уведомление «Скоро» остаётся (§13).
                            section = .calendar
                            workspace.calendar?.open(eventID: key.eventID, occStart: key.occStart)
                        default:
                            if let s = ReminderService.snoozeActions.first(where: { $0.id == action }) {
                                Task { await reminders.snooze(key, minutes: s.minutes) }
                            }
                        }
                    }
                }
                NotificationRouter.shared.openRef = { [weak mail] in mail?.selectedID }
                mail.live.forEach { $0.start() }
                await mail.sync()
                await Notifier.requestPermissionIfNeeded()
                // Push (§7): токен APNs и регистрация ddmail-учёток — после
                // разрешения на уведомления, при каждом выходе на экран.
                PushService.shared.attach(workspace)
                await workspace.reminders?.refresh()
            }
    }

    @ViewBuilder private func content(_ s: AppSection) -> some View {
        switch s {
        case .mail:
            if let mail = workspace.mail {
                MailView(model: mail, app: app, workspace: workspace)
            } else {
                SectionPlaceholder(section: s, text: workspace.cacheError ?? "Кэш почты не открылся.")
            }
        case .calendar:
            if let cal = workspace.calendar { CalendarScreen(model: cal) } else { SectionPlaceholder(section: s) }
        case .tasks:
            if let t = workspace.tasks { TasksView(model: t) } else { SectionPlaceholder(section: s) }
        case .contacts:
            if let c = workspace.contacts {
                ContactsView(model: c, avatars: workspace.mail?.avatars) { addr in
                    // «Написать» — беседа с этим адресом в почте: есть — она, нет — новая (как десктоп).
                    section = .mail
                    workspace.mail?.write(to: addr)
                }
            } else {
                SectionPlaceholder(section: s)
            }
        }
    }
}

extension AppShell {
    /// Действия этого окна для меню и клавиатуры.
    var mailActions: MailActions? {
        guard let mail = workspace.mail else { return nil }
        let open = mail.selected
        return MailActions(
            // Новое письмо — только через поиск (mobile-layout.md): ⌘N ставит фокус в поле поиска.
            newMail: { section = .mail; mail.search.requestFocus() },
            reply: open.map { c in { section = .mail; mail.compose.replyTo = mail.conversation.lastIncoming ?? mail.conversation.last; _ = c } },
            forward: (mail.conversation.lastIncoming ?? mail.conversation.last).map { b in { mail.compose.openForward(b) } },
            delete: open.map { c in { mail.confirmDelete = [c.ref] } },
            refresh: { Task { await mail.sync() } },
            go: { section = $0 },
            settings: { settingsShown = true }
        )
    }
}

/// Плашка «нужен вход»: почта учётки не обновляется, пока не войти заново.
struct SessionDeadBanner: View {
    /// Учёток несколько — какая именно.
    let title: String?
    let relogin: () -> Void

    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: "person.crop.circle.badge.exclamationmark")
                .font(.title3)
            Text(title.map { "\($0): нужен вход, почта не обновляется." } ?? "Сессия истекла: почта не обновляется.")
                .font(.subheadline)
            Spacer(minLength: 8)
            Button("Войти", action: relogin)
                .buttonStyle(.borderedProminent)
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
        .background(.orange.opacity(0.18))
        .accessibilityElement(children: .combine)
    }
}

/// Раздел, которому нужна учётка ddmail (календарь, задачи, контакты), а её нет.
private struct SectionPlaceholder: View {
    let section: AppSection
    var text = "Раздел ведёт сервер ddmail или CalDAV/CardDAV учётки. Добавьте их в «Настройки → Учётки»."

    var body: some View {
        VStack(spacing: 0) {
            SectionBar {
                Text(section.title).font(.headline)
                Spacer()
            }
            ContentUnavailableView {
                Label(section.title, systemImage: section.symbol)
            } description: {
                Text(text)
            }
            .frame(maxHeight: .infinity)
        }
    }
}

/// Порядок — как в меню бургера десктопа (`view-menu-open`).
enum AppSection: String, CaseIterable, Identifiable {
    case mail, calendar, contacts, tasks

    var id: Self { self }

    var title: String {
        switch self {
        case .mail: "Почта"
        case .calendar: "Календарь"
        case .tasks: "Задачи"
        case .contacts: "Контакты"
        }
    }

    var symbol: String {
        switch self {
        case .mail: "envelope"
        case .calendar: "calendar"
        case .tasks: "checklist"
        case .contacts: "person.crop.circle"
        }
    }
}
