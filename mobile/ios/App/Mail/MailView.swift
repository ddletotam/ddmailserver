import DDMailAPI
import DDMailSync
import SwiftUI

/// Почта (контракт §5–§6): список и беседа рядом на широком окне, стек на
/// узком — `NavigationSplitView` складывается сам по ширине.
struct MailView: View {
    @Bindable var model: MailModel
    let app: AppModel
    let connection: Connection
    @State private var picking = false
    @State private var confirmDelete: Set<String>?
    @State private var confirmSpam: Set<String>?
    @State private var moving: Set<String>?
    @State private var confirmLogout = false
    @Environment(\.horizontalSizeClass) private var sizeClass

    var body: some View {
        NavigationSplitView {
            listPane
                .navigationSplitViewColumnWidth(min: 320, ideal: 380, max: 480)
        } detail: {
            if let c = model.selected {
                ConversationView(conversation: c, model: model.conversation) { confirmSpam = [c.id] }
            } else {
                ContentUnavailableView("Беседа не выбрана", systemImage: "bubble.left.and.bubble.right")
            }
        }
        .tint(Palette.accent)
        .overlay(alignment: .bottom) { NoticeBanner(text: $model.notice) }
        .confirmationDialog(deleteTitle, isPresented: present($confirmDelete), titleVisibility: .visible) {
            Button("Удалить", role: .destructive) { if let ids = confirmDelete { model.delete(ids) } }
        } message: {
            Text("Письма будут удалены на сервере; у внешних ящиков — и в исходном ящике.")
        }
        .confirmationDialog("В спам?", isPresented: present($confirmSpam), titleVisibility: .visible) {
            Button("Заблокировать адрес", role: .destructive) { if let ids = confirmSpam { model.reportSpam(ids, scope: "address") } }
            Button("Заблокировать домен", role: .destructive) { if let ids = confirmSpam { model.reportSpam(ids, scope: "domain") } }
        } message: {
            Text(spamMessage)
        }
        .confirmationDialog("В папку", isPresented: present($moving), titleVisibility: .visible) {
            Button("Во «Входящие»") { if let ids = moving { model.move(ids, to: nil) } }
            ForEach(model.folderTargets, id: \.id) { f in
                Button(f.icon.isEmpty ? f.name : "\(f.icon) \(f.name)") { if let ids = moving { model.move(ids, to: f.id) } }
            }
        }
        .confirmationDialog("Выйти из учётной записи?", isPresented: $confirmLogout, titleVisibility: .visible) {
            Button("Выйти", role: .destructive, action: app.logout)
        } message: {
            Text("Сессия и почта этой учётной записи будут удалены с устройства.")
        }
        .onAppear(perform: restoreLast)
        .onChange(of: model.loaded) { restoreLast() }
    }

    // MARK: Список

    private var listPane: some View {
        // Читаем наблюдаемое здесь, в теле: замыкания `ForEach` и
        // `safeAreaInset` SwiftUI вызывает лениво, вне отслеживания
        // `@Observable`, — прочитанное в них не перерисовывало бы экран
        // (так и было: цвета и табы, пришедшие после первого кадра, не появлялись).
        let colors = model.colors
        let tabs = model.list.searching ? [] : model.list.tabs
        let convs = model.list.conversations
        let selectedID = model.selectedID
        return Group {
            if picking {
                List(selection: $model.picked) { rows(convs, colors: colors, selectedID: nil) }
            } else {
                List(selection: $model.selectedID) { rows(convs, colors: colors, selectedID: selectedID) }
            }
        }
        .listStyle(.plain)
        .pickingMode(picking)
        .safeAreaInset(edge: .top, spacing: 0) {
            if !tabs.isEmpty {
                FolderTabs(tabs: tabs, active: $model.activeFolderID)
            }
        }
        .overlay { emptyState }
        .refreshable { await model.sync() }
        .searchable(text: $model.query, prompt: "Имя, адрес или тема")
        .navigationTitle(title)
        .toolbar { listToolbar }
        .onChange(of: model.list.active) { picking = false }
    }

    @ViewBuilder private func rows(_ convs: [Conversation], colors: IdentityColors, selectedID: String?) -> some View {
        ForEach(convs) { c in
            ConversationRow(conversation: c, colors: colors, selected: c.id == selectedID)
                .tag(c.id)
                .swipeActions(edge: .leading, allowsFullSwipe: true) {
                    if !c.spam {
                        Button {
                            model.toggleRead(c)
                        } label: {
                            Label(c.unreadCount > 0 ? "Прочитано" : "Непрочитано",
                                  systemImage: c.unreadCount > 0 ? "envelope.open" : "envelope.badge")
                        }
                        .tint(.blue)
                    }
                }
                .swipeActions(edge: .trailing, allowsFullSwipe: false) {
                    if !c.spam {
                        Button { moving = [c.id] } label: { Label("В папку", systemImage: "folder") }
                            .tint(.indigo)
                    }
                }
                .contextMenu { contextMenu(c) }
        }
    }

    @ViewBuilder private func contextMenu(_ c: Conversation) -> some View {
        if c.spam {
            Button("Не спам", systemImage: "tray.and.arrow.down") { model.notSpam([c.id], trust: false) }
            Button("Не спам и доверять отправителю", systemImage: "checkmark.shield") { model.notSpam([c.id], trust: true) }
        } else {
            Button(c.unreadCount > 0 ? "Прочитано" : "Непрочитано",
                   systemImage: c.unreadCount > 0 ? "envelope.open" : "envelope.badge") { model.toggleRead(c) }
            Button("В папку…", systemImage: "folder") { moving = [c.id] }
            Divider()
            Button("В спам…", systemImage: "hand.raised", role: .destructive) { confirmSpam = [c.id] }
            Button("Удалить…", systemImage: "trash", role: .destructive) { confirmDelete = [c.id] }
        }
    }

    @ToolbarContentBuilder private var listToolbar: some ToolbarContent {
        if picking {
            ToolbarItem(placement: .cancellationAction) {
                Button("Готово", systemImage: "xmark") { model.picked.removeAll(); picking = false }
            }
            ToolbarItem(placement: .principal) {
                Text("Выбрано: \(model.picked.count)").font(.headline)
            }
            ToolbarItemGroup(placement: Self.actionsPlacement) { pickingActions }
        } else {
            ToolbarItem(placement: .primaryAction) {
                Menu("Ещё", systemImage: "ellipsis.circle") {
                    Button("Выбрать", systemImage: "checkmark.circle") { picking = true }
                    Button("Спам", systemImage: "hand.raised") { model.openSpam() }
                    Divider()
                    Text(connection.session.accountKey)
                    Button("Выйти", systemImage: "rectangle.portrait.and.arrow.right", role: .destructive) { confirmLogout = true }
                }
            }
            if model.syncing {
                ToolbarItem(placement: .status) { ProgressView().controlSize(.small) }
            }
        }
    }

    /// Панель множественного выбора (§5): иконки, подписи — для озвучки.
    @ViewBuilder private var pickingActions: some View {
        let ids = model.picked
        Button("Выбрать все", systemImage: "checklist") { model.picked = Set(model.list.conversations.map(\.id)) }
        Spacer()
        if model.list.inSpam {
            Button("Не спам", systemImage: "tray.and.arrow.down") { model.notSpam(ids, trust: false); picking = false }
                .disabled(ids.isEmpty)
            Button("Не спам и доверять отправителю", systemImage: "checkmark.shield") { model.notSpam(ids, trust: true); picking = false }
                .disabled(ids.isEmpty)
        } else {
            Button("Прочитано", systemImage: "envelope.open") { model.markRead(ids, read: true); picking = false }
                .disabled(ids.isEmpty)
            Button("Непрочитано", systemImage: "envelope.badge") { model.markRead(ids, read: false); picking = false }
                .disabled(ids.isEmpty)
            Button("В папку", systemImage: "folder") { moving = ids }
                .disabled(ids.isEmpty)
            Button("В спам", systemImage: "hand.raised") { confirmSpam = ids }
                .disabled(ids.isEmpty)
            Button("Удалить", systemImage: "trash", role: .destructive) { confirmDelete = ids }
                .disabled(ids.isEmpty)
        }
    }

    /// Действия выбора: на iPhone и iPad — нижняя панель, на Маке — панель окна.
    private static var actionsPlacement: ToolbarItemPlacement {
        #if os(iOS)
        .bottomBar
        #else
        .automatic
        #endif
    }

    @ViewBuilder private var emptyState: some View {
        if model.loaded && model.list.conversations.isEmpty {
            if model.list.searching {
                ContentUnavailableView.search(text: model.query)
            } else if model.list.inSpam {
                ContentUnavailableView("Спама нет", systemImage: "hand.raised")
            } else {
                ContentUnavailableView(model.list.active == nil ? "Входящих нет" : "В папке пусто", systemImage: "tray")
            }
        } else if !model.loaded {
            ProgressView()
        }
    }

    private var title: String {
        guard let i = model.list.active, model.doc.folders.indices.contains(i) else { return "Входящие" }
        return model.doc.folders[i].name
    }

    private var deleteTitle: String {
        let s = model.summary(confirmDelete ?? [])
        return "Удалить бесед: \(s.conversations), писем в них: \(s.messages)?"
    }

    private var spamMessage: String {
        let s = model.summary(confirmSpam ?? [])
        return "Бесед: \(s.conversations), писем в них: \(s.messages). Письма будут удалены, отправитель — заблокирован. Домен останавливает спам с меняющихся адресов одного домена."
    }

    /// На раскладке, где беседа видна сразу, — открыть ту, на которой закончили (§5).
    private func restoreLast() {
        guard sizeClass == .regular, model.selectedID == nil, model.loaded,
              let id = model.lastConversationID, model.all.contains(where: { $0.id == id })
        else { return }
        model.open(id)
    }

    private func present(_ v: Binding<Set<String>?>) -> Binding<Bool> {
        Binding(get: { v.wrappedValue != nil }, set: { if !$0 { v.wrappedValue = nil } })
    }
}

/// Табы папок над списком (§5): ничего не выбрано = «Входящие»; повторное
/// касание выбранного таба возвращает во «Входящие».
private struct FolderTabs: View {
    let tabs: [FolderTab]
    @Binding var active: String?

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 8) {
                ForEach(tabs) { t in
                    let on = t.id == active
                    Button {
                        active = on ? nil : t.id
                    } label: {
                        HStack(spacing: 6) {
                            if !t.icon.isEmpty { Text(t.icon) }
                            Text(t.name).lineLimit(1)
                            if t.unread > 0 {
                                Text("\(t.unread)")
                                    .font(.caption.weight(.semibold))
                                    .padding(.horizontal, 6)
                                    .padding(.vertical, 1)
                                    .background(Capsule().fill(on ? Color.white.opacity(0.3) : Palette.accent.opacity(0.18)))
                            }
                        }
                        .font(.subheadline.weight(on ? .semibold : .regular))
                        .padding(.horizontal, 12)
                        .padding(.vertical, 7)
                        .foregroundStyle(on ? Color.white : Color.primary)
                        .background(Capsule().fill(on ? Palette.accent : Color.secondary.opacity(0.12)))
                    }
                    .buttonStyle(.plain)
                    .accessibilityAddTraits(on ? .isSelected : [])
                }
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 8)
        }
        .background(.bar)
    }
}

/// Строка беседы: аватар, имя, тема, дата, непрочитанное; фон — пастель
/// группы адреса получателя (§5), открытая — зелёная с белым текстом.
struct ConversationRow: View {
    let conversation: Conversation
    let colors: IdentityColors
    let selected: Bool

    var body: some View {
        let c = conversation
        let unread = c.unreadCount > 0
        HStack(alignment: .top, spacing: 12) {
            Avatar(name: c.label, seed: c.counterparts.first?.addr ?? c.id)
            VStack(alignment: .leading, spacing: 3) {
                HStack(alignment: .firstTextBaseline) {
                    Text(c.label.isEmpty ? (c.counterparts.first?.addr ?? "—") : c.label)
                        .font(.body.weight(unread ? .semibold : .regular))
                        .lineLimit(1)
                    Spacer(minLength: 8)
                    Text(RowDate.format(c.lastDateTs))
                        .font(.footnote)
                        .foregroundStyle(selected ? .white.opacity(0.85) : .secondary)
                }
                HStack(alignment: .firstTextBaseline) {
                    Text(c.lastSubject.isEmpty ? "(без темы)" : c.lastSubject)
                        .font(.subheadline)
                        .foregroundStyle(selected ? .white.opacity(0.9) : .secondary)
                        .lineLimit(2)
                    Spacer(minLength: 8)
                    // Спам не зазывает: числа непрочитанных нет (§5).
                    if unread && !c.spam {
                        Text("\(c.unreadCount)")
                            .font(.caption.weight(.semibold))
                            .foregroundStyle(.white)
                            .padding(.horizontal, 7)
                            .padding(.vertical, 2)
                            .background(Capsule().fill(selected ? Color.white.opacity(0.3) : Palette.accent))
                    }
                }
            }
        }
        .foregroundStyle(selected ? Color.white : Color.primary)
        .padding(.vertical, 4)
        .listRowBackground(selected ? Palette.accent : Color(argb: colors.color(of: c.receivedBy)))
        .accessibilityElement(children: .combine)
    }
}

/// Инициалы на цветном круге. Картинки аватаров — следующим шагом (§5).
struct Avatar: View {
    let name: String
    let seed: String

    var body: some View {
        let initials = name.split(separator: " ").prefix(2).compactMap(\.first).map(String.init).joined().uppercased()
        Circle()
            .fill(Palette.avatar(seed))
            .frame(width: 40, height: 40)
            .overlay(Text(initials.isEmpty ? "?" : initials).font(.subheadline.weight(.semibold)).foregroundStyle(.white))
            .accessibilityHidden(true)
    }
}

/// Плашка итога действия: появляется снизу и уходит сама через 3 с.
struct NoticeBanner: View {
    @Binding var text: String?

    var body: some View {
        if let t = text {
            Text(t)
                .font(.subheadline)
                .padding(.horizontal, 16)
                .padding(.vertical, 10)
                .background(.thinMaterial, in: Capsule())
                .padding(.bottom, 24)
                .transition(.move(edge: .bottom).combined(with: .opacity))
                .task(id: t) {
                    try? await Task.sleep(for: .seconds(3))
                    withAnimation { text = nil }
                }
                .accessibilityAddTraits(.updatesFrequently)
        }
    }
}

enum RowDate {
    /// Сегодня — время, на этой неделе — день недели, этот год — день и
    /// месяц, иначе — дата целиком.
    static func format(_ ts: Int64) -> String {
        guard ts > 0 else { return "" }
        let d = Date(timeIntervalSince1970: TimeInterval(ts))
        let cal = Calendar.current
        if cal.isDateInToday(d) { return d.formatted(date: .omitted, time: .shortened) }
        if let days = cal.dateComponents([.day], from: cal.startOfDay(for: d), to: cal.startOfDay(for: .now)).day, days < 7 {
            return d.formatted(.dateTime.weekday(.abbreviated))
        }
        if cal.isDate(d, equalTo: .now, toGranularity: .year) { return d.formatted(.dateTime.day().month(.abbreviated)) }
        return d.formatted(date: .numeric, time: .omitted)
    }
}

enum Palette {
    /// Выделение и акцент — `#10b981`, как выделение беседы на десктопе и Android.
    static let accent = Color(red: 0x10 / 255, green: 0xB9 / 255, blue: 0x81 / 255)

    /// Цвет круга аватара — стабильный по адресу.
    static func avatar(_ seed: String) -> Color {
        let hues: [Double] = [0.0, 0.08, 0.14, 0.33, 0.47, 0.55, 0.62, 0.72, 0.8, 0.9]
        var h: UInt32 = 2166136261
        for b in seed.lowercased().utf8 { h = (h ^ UInt32(b)) &* 16777619 }
        return Color(hue: hues[Int(h % UInt32(hues.count))], saturation: 0.45, brightness: 0.78)
    }
}

extension Color {
    /// `0xAARRGGBB` из `IdentityColors`.
    init(argb: UInt32) {
        self.init(
            .sRGB,
            red: Double((argb >> 16) & 0xFF) / 255,
            green: Double((argb >> 8) & 0xFF) / 255,
            blue: Double(argb & 0xFF) / 255,
            opacity: Double((argb >> 24) & 0xFF) / 255
        )
    }
}

private extension View {
    /// Режим выбора: на iPhone и iPad — системный режим правки списка (кружки
    /// у строк); на Маке список с множественным выбором выбирает Cmd/Shift-кликом сам.
    @ViewBuilder func pickingMode(_ on: Bool) -> some View {
        #if os(iOS)
        environment(\.editMode, .constant(on ? .active : .inactive))
        #else
        self
        #endif
    }
}
