import DDMailAPI
import DDMailSync
import SwiftUI

/// Почта (контракт §5–§6, `docs/mobile-layout.md`): левая колонка — строка
/// «бургер + поиск», полоса табов папок, диалоги; справа беседа. На узком
/// экране колонка — весь экран, беседа открывается поверх.
/// `NavigationSplitView` складывается сам по ширине; третьей колонки нет.
struct MailView: View {
    @Bindable var model: MailModel
    let app: AppModel
    let workspace: Workspace
    /// Режим выбора — состояние модели: от него зависит, что делает выбор строки.
    private var picking: Bool {
        get { model.picking }
        nonmutating set { model.picking = newValue }
    }
    @State private var confirmSpam: Set<String>?
    @State private var moving: Set<String>?
    /// Открытая настройка папки («Настроить…», «Новая папка»).
    @State private var editingFolder: FolderRef?
    /// Правый край колонки списка в координатах окна (Мак, см. `detailColumn`).
    @State private var listEdge: CGFloat = 0
    @Bindable private var settings = AppSettings.shared
    @Environment(\.horizontalSizeClass) private var sizeClass
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        // Условия показа окон — читаем здесь, в теле: замыкание `Binding(get:)`
        // SwiftUI вызывает лениво, вне отслеживания `@Observable` (см. контракт §0).
        let askPrefetch = model.loaded && settings.prefetch == nil
        let composeOpen = model.compose.sheet != nil
        let selected = model.selected
        let composingTo = model.composingTo
        return NavigationSplitView(columnVisibility: .constant(.all)) {
            listPane
                .navigationSplitViewColumnWidth(min: 320, ideal: 380, max: 480)
        } detail: {
            detailColumn {
                if let to = composingTo {
                    NewConversationView(address: to, model: model.conversation, compose: model.compose)
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                } else if let c = selected {
                    ConversationView(conversation: c, model: model.conversation, compose: model.compose) { confirmSpam = [c.ref] }
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                } else {
                    ContentUnavailableView("Беседа не выбрана", systemImage: "bubble.left.and.bubble.right")
                }
            }
        }
        // Колонки рядом всегда: ни скрыть список, ни отдельной кнопки панели
        // (на Маке её убирает колонка списка — `sectionRoot()`).
        .navigationSplitViewStyle(.balanced)
        .toolbar(removing: .sidebarToggle)
        .tint(Palette.accent)
        .overlay(alignment: .bottom) { NoticeBanner(text: $model.notice) }
        .modifier(DeleteConfirmation(model: model, ids: $model.confirmDelete))
        .modifier(SpamConfirmation(model: model, ids: $confirmSpam))
        .confirmationDialog("В папку", isPresented: present($moving), titleVisibility: .visible) {
            Button("Во «Входящие»") { if let ids = moving { model.move(ids, to: nil) } }
            ForEach(model.folderTargets, id: \.id) { f in
                Button(f.icon.isEmpty ? f.name : "\(f.icon) \(f.name)") { if let ids = moving { model.move(ids, to: f.id) } }
            }
        }
        .sheet(item: $editingFolder) { f in
            NavigationStack {
                FolderEditorView(mail: model, workspace: workspace, folderID: f.id)
                    .toolbar {
                        ToolbarItem(placement: .confirmationAction) {
                            Button("Готово", systemImage: "checkmark") { editingFolder = nil }
                        }
                    }
            }
            #if os(macOS)
            .frame(minWidth: 480, minHeight: 520)
            #endif
        }
        // Пересылка — отдельным окном, как раньше (новое письмо — через поиск).
        .sheet(isPresented: Binding(get: { composeOpen }, set: { if !$0 { model.compose.sheet = nil } })) {
            ComposeSheetView(model: model.compose)
        }
        // Один вопрос после первого входа: ответа по умолчанию нет (§4).
        .alert("Загружать письма заранее?", isPresented: Binding(get: { askPrefetch }, set: { _ in })) {
            ForEach(AppSettings.Prefetch.allCases) { p in
                Button(p.title) {
                    settings.prefetch = p
                    model.startPrefetch()
                }
            }
        } message: {
            Text("Тогда беседы открываются сразу и без сети. Изменить — «Настройки → Загрузка писем».")
        }
        .onAppear(perform: restoreLast)
        .onChange(of: model.loaded) { restoreLast() }
    }

    /// Колонка беседы. На macOS `NavigationSplitView` тянет колонку под
    /// боковую и считал отступ слева с лишней шириной (замер при прежней
    /// боковой панели разделов: колонка с x=770, лента — с 914). Отступ
    /// берём из измерения — правого края колонки списка, — а не из
    /// безопасной области; без лишней панели он нулевой.
    @ViewBuilder private func detailColumn(@ViewBuilder _ content: () -> some View) -> some View {
        #if os(macOS)
        let body = content()
        GeometryReader { geo in
            body.padding(.leading, max(listEdge - geo.frame(in: .global).minX, 0))
        }
        .ignoresSafeArea(.container, edges: .leading)
        #else
        content()
        #endif
    }

    // MARK: Список

    private var listPane: some View {
        // Читаем наблюдаемое здесь, в теле: замыкания `ForEach` и
        // `safeAreaInset` SwiftUI вызывает лениво, вне отслеживания
        // `@Observable`, — прочитанное в них не перерисовывало бы экран
        // (так и было: цвета и табы, пришедшие после первого кадра, не появлялись).
        let colors = model.colors
        let tabs = model.list.tabs
        let activeID = model.activeFolderID
        let inboxUnread = model.list.inboxUnread
        let convs = model.list.conversations
        let selectedID = model.selectedID
        let composingTo = model.composingTo
        let searchOpen = model.search.isOpen
        let syncing = model.syncing
        let picked = model.picked
        let pickedCount = picked.count
        // «Нужен вход» — внизу колонки, под списком (S-01, Android §6): над
        // строкой «бургер + поиск» ничего нет (mobile-layout.md).
        let dead = workspace.dead.map { ($0.account.key, $0.account.title) }
        let manyAccounts = workspace.links.count > 1
        let hiddenCount = model.foldersOn ? model.doc.hiddenCount : 0
        let showHidden = model.showHidden
        return VStack(spacing: 0) {
            if picking {
                PickingBar(count: pickedCount,
                           selectAll: { model.picked = Set(model.list.conversations.map(\.ref)) },
                           done: { model.picked.removeAll(); picking = false })
            } else {
                SectionBar {
                    MailSearchField(search: model.search, syncing: syncing)
                }
            }
            // Папок нет — полосы нет; все скрыты — полоса есть (пустая, с меню
            // «Показать скрытые»), как у десктопа.
            if !tabs.isEmpty || hiddenCount > 0 {
                FolderTabs(
                    tabs: tabs, active: activeID, inboxUnread: inboxUnread,
                    menu: FolderTabMenu(
                        hiddenCount: hiddenCount, showHidden: showHidden,
                        markRead: { model.markFolderRead($0) },
                        configure: { editingFolder = FolderRef(id: $0) },
                        delete: { model.deleteFolder($0) },
                        add: { Task { if let id = await model.addFolder() { editingFolder = FolderRef(id: id) } } },
                        toggleHidden: { model.showHidden.toggle() }
                    )
                ) { model.activeFolderID = $0 }
            }
            Group {
                if picking {
                    // Отметки — свои, без `List(selection:)`: непустой выбор
                    // списка в свёрнутом сплите (iPhone) толкает колонку беседы —
                    // «Выбрать» уводил на пустую беседу (B-01); `preferredCompactColumn`
                    // этого не держит. Касание (клик) строки — отметить или снять.
                    List { rows(convs, colors: colors, selectedID: nil, picked: picked, picking: true) }
                } else {
                    ScrollViewReader { proxy in
                        List(selection: $model.openRow) {
                            if let to = composingTo { NewConversationRow(address: to).tag(MailModel.newPrefix + to).id(MailModel.newPrefix + to) }
                            rows(convs, colors: colors, selectedID: selectedID, picked: [], picking: false)
                        }
                        // Открытая беседа видна в списке: «Новая беседа» и беседа,
                        // которой её сменила отправка, встают сверху, а список
                        // держит позицию по прежней первой строке — строка
                        // оказывалась выше края (так «пропадала» беседа на Android).
                        .onChange(of: selectedID) { _, id in
                            guard let id else { return }
                            withAnimation { proxy.scrollTo(id) }
                        }
                    }
                }
            }
            .listStyle(.plain)
            .overlay { if !searchOpen { emptyState } }
            .refreshable { await model.sync() }
            // Выпадашка поиска — поверх списка, как у десктопа.
            .overlay(alignment: .top) {
                if searchOpen { SearchDropdown(search: model.search, avatars: model.avatars) }
            }
            .safeAreaInset(edge: .bottom, spacing: 0) {
                VStack(spacing: 0) {
                    ForEach(dead, id: \.0) { key, title in
                        SessionDeadBanner(title: dead.count > 1 || manyAccounts ? title : nil) { app.relogin(key) }
                    }
                    if picking { pickingActions }
                }
            }
        }
        .sectionRoot()
        .onGeometryChange(for: CGFloat.self) { $0.frame(in: .global).maxX } action: { listEdge = $0 }
        .onChange(of: model.list.active) { picking = false }
        // Снята последняя отметка (или действие над выбором выполнено) — выбор
        // закончен (Android §5, M-17).
        .onChange(of: pickedCount) { _, n in if n == 0 { picking = false } }
    }

    /// Отметить строку или снять отметку (режим выбора).
    private func togglePick(_ ref: String) {
        if model.picked.contains(ref) { model.picked.remove(ref) } else { model.picked.insert(ref) }
    }

    @ViewBuilder private func rows(_ convs: [Conversation], colors: IdentityColors, selectedID: String?, picked: Set<String>, picking: Bool) -> some View {
        ForEach(convs, id: \.ref) { c in
            ConversationRow(conversation: c, colors: colors, selected: c.ref == selectedID, picked: picked.contains(c.ref),
                            avatars: model.avatars)
                .pickable(picking) { togglePick(c.ref) }
                .tag(c.ref)
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
                        Button { moving = [c.ref] } label: { Label("В папку", systemImage: "folder") }
                            .tint(.indigo)
                    }
                }
                .contextMenu { contextMenu(c, spamOK: model.spamSupport([c.ref]) != .none) }
        }
    }

    /// Меню строки (долгое нажатие, правый клик) — как меню строки десктопа;
    /// здесь же вход в множественный выбор.
    @ViewBuilder private func contextMenu(_ c: Conversation, spamOK: Bool) -> some View {
        if c.spam {
            Button("Не спам", systemImage: "tray.and.arrow.down") { model.notSpam([c.ref], trust: false) }
            if model.canTrust([c.ref]) {
                Button("Не спам и доверять отправителю", systemImage: "checkmark.shield") { model.notSpam([c.ref], trust: true) }
            }
        } else {
            Button(c.unreadCount > 0 ? "Прочитано" : "Непрочитано",
                   systemImage: c.unreadCount > 0 ? "envelope.open" : "envelope.badge") { model.toggleRead(c) }
            Button("В папку…", systemImage: "folder") { moving = [c.ref] }
            #if os(macOS)
            Button("Открыть в новом окне", systemImage: "macwindow.badge.plus") { openWindow(id: "conversation", value: c.ref) }
            #endif
            Divider()
            if spamOK {
                Button("В спам…", systemImage: "hand.raised", role: .destructive) { confirmSpam = [c.ref] }
            }
            Button("Удалить…", systemImage: "trash", role: .destructive) { model.confirmDelete = [c.ref] }
        }
        Divider()
        Button("Выбрать", systemImage: "checkmark.circle") {
            model.picked = [c.ref]
            picking = true
        }
    }

    /// Панель множественного выбора (§5) под списком: иконки, подписи — для озвучки.
    @ViewBuilder private var pickingActions: some View {
        let ids = model.picked
        HStack(spacing: 22) {
            if model.list.inSpam {
                Button("Не спам", systemImage: "tray.and.arrow.down") { model.notSpam(ids, trust: false); picking = false }
                    .disabled(ids.isEmpty)
                Button("Не спам и доверять отправителю", systemImage: "checkmark.shield") { model.notSpam(ids, trust: true); picking = false }
                    .disabled(ids.isEmpty || !model.canTrust(ids))
            } else {
                Button("Прочитано", systemImage: "envelope.open") { model.markRead(ids, read: true); picking = false }
                    .disabled(ids.isEmpty)
                Button("Непрочитано", systemImage: "envelope.badge") { model.markRead(ids, read: false); picking = false }
                    .disabled(ids.isEmpty)
                Button("В папку", systemImage: "folder") { moving = ids }
                    .disabled(ids.isEmpty)
                Button("В спам", systemImage: "hand.raised") { confirmSpam = ids }
                    .disabled(ids.isEmpty || model.spamSupport(ids) == .none)
                Button("Удалить", systemImage: "trash", role: .destructive) { model.confirmDelete = ids }
                    .disabled(ids.isEmpty)
            }
        }
        .labelStyle(.iconOnly)
        .font(.title3)
        .buttonStyle(.borderless)
        .frame(maxWidth: .infinity)
        .padding(.vertical, 10)
        .background(.bar)
    }

    @ViewBuilder private var emptyState: some View {
        if model.loaded && model.list.conversations.isEmpty && model.composingTo == nil {
            if model.list.inSpam {
                ContentUnavailableView("Спама нет", systemImage: "hand.raised")
            } else {
                ContentUnavailableView(model.list.active == nil ? "Входящих нет" : "В папке пусто", systemImage: "tray")
            }
        } else if !model.loaded {
            ProgressView()
        }
    }

    /// На раскладке, где беседа видна сразу, — открыть ту, на которой закончили
    /// (§5). На Маке колонки рядом всегда, а `horizontalSizeClass` там `nil` (S-03).
    private func restoreLast() {
        #if os(macOS)
        let wide = true
        #else
        let wide = sizeClass == .regular
        #endif
        guard wide, model.selectedID == nil, model.loaded,
              let id = model.lastConversationID, model.all.contains(where: { $0.ref == id })
        else { return }
        model.open(id)
    }

    private func present(_ v: Binding<Set<String>?>) -> Binding<Bool> {
        // Значение читается сразу (в теле), а не в ленивом замыкании.
        let shown = v.wrappedValue != nil
        return Binding(get: { shown }, set: { if !$0 { v.wrappedValue = nil } })
    }
}

/// Подтверждение удаления бесед (§5): API восстановления нет. Общее у
/// главного окна и окна беседы (Мак); `done` — после удаления.
struct DeleteConfirmation: ViewModifier {
    let model: MailModel
    @Binding var ids: Set<String>?
    var done: () -> Void = {}

    func body(content: Content) -> some View {
        // Значения — сразу, в теле (контракт §0).
        let current = ids
        let s = model.summary(current ?? [])
        content.confirmationDialog("Удалить бесед: \(s.conversations), писем в них: \(s.messages)?",
                                   isPresented: Binding(get: { current != nil }, set: { if !$0 { ids = nil } }),
                                   titleVisibility: .visible) {
            Button("Удалить", role: .destructive) {
                if let current { model.delete(current) }
                done()
            }
        } message: {
            Text("Письма будут удалены на сервере; у внешних ящиков — и в исходном ящике.")
        }
    }
}

/// «В спам?» (§5): ddmail блокирует адрес или домен правилом сервера; у
/// внешних учёток правил нет — одна кнопка, письма уходят в Junk (§4).
/// Общее у главного окна и окна беседы (Мак).
struct SpamConfirmation: ViewModifier {
    let model: MailModel
    @Binding var ids: Set<String>?
    var done: () -> Void = {}

    func body(content: Content) -> some View {
        let current = ids
        let kind = model.spamSupport(current ?? [])
        let s = model.summary(current ?? [])
        let message = kind == .rules
            ? "Бесед: \(s.conversations), писем в них: \(s.messages). Письма будут удалены, отправитель — заблокирован. Домен останавливает спам с меняющихся адресов одного домена."
            : "Бесед: \(s.conversations), писем в них: \(s.messages). Письма будут перенесены в папку «Спам» почтового ящика."
        content.confirmationDialog("В спам?", isPresented: Binding(get: { current != nil }, set: { if !$0 { ids = nil } }),
                                   titleVisibility: .visible) {
            if kind == .rules {
                Button("Заблокировать адрес", role: .destructive) { report(current, "address") }
                Button("Заблокировать домен", role: .destructive) { report(current, "domain") }
            } else {
                Button("В спам", role: .destructive) { report(current, "address") }
            }
        } message: {
            Text(message)
        }
    }

    private func report(_ ids: Set<String>?, _ scope: String) {
        guard let ids else { return }
        model.reportSpam(ids, scope: scope)
        done()
    }
}

/// Строка множественного выбора на месте строки поиска: «Готово», сколько
/// выбрано, «Выбрать все».
private struct PickingBar: View {
    let count: Int
    let selectAll: () -> Void
    let done: () -> Void

    var body: some View {
        HStack(spacing: 8) {
            Button("Готово", systemImage: "xmark", action: done)
                .accessibilityIdentifier("picking.done")
                .labelStyle(.iconOnly)
                .font(.system(size: 17, weight: .medium))
                .frame(width: 36, height: 36)
                .contentShape(Rectangle())
            Text("Выбрано: \(count)").font(.headline)
                .accessibilityIdentifier("picking.count")
            Spacer()
            Button("Выбрать все", action: selectAll)
        }
        .buttonStyle(.borderless)
        .padding(.leading, 8)
        .padding(.trailing, 12)
        .frame(minHeight: 52)
    }
}

/// Строка новой беседы над диалогами, пока письмо не ушло (десктоп
/// `refresh_sidebar` с синтетической строкой).
private struct NewConversationRow: View {
    let address: String

    var body: some View {
        HStack(spacing: 12) {
            Circle()
                .fill(Color.white.opacity(0.25))
                .frame(width: 40, height: 40)
                .overlay(Image(systemName: "square.and.pencil").foregroundStyle(.white))
            VStack(alignment: .leading, spacing: 3) {
                Text(address).font(.body.weight(.semibold)).lineLimit(1)
                Text("Новая беседа").font(.subheadline).opacity(0.9)
            }
        }
        .foregroundStyle(.white)
        .padding(.vertical, 4)
        .listRowBackground(Palette.accent)
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier("conv.new")
    }
}

/// Папка, чья настройка открыта окном.
struct FolderRef: Identifiable, Hashable {
    let id: String
}

/// Меню полосы табов (десктоп §4 «Папки диалогов»): на табе папки — «Все
/// прочитаны», «Настроить…», «Удалить» (подменю — подтверждение); везде —
/// «Новая папка» и «Показать скрытые (N)» / «Не показывать скрытые».
struct FolderTabMenu {
    let hiddenCount: Int
    let showHidden: Bool
    let markRead: (String) -> Void
    let configure: (String) -> Void
    let delete: (String) -> Void
    let add: () -> Void
    let toggleHidden: () -> Void
}

/// Табы папок над списком — как у десктопа (§5, десктоп §4 «Папки диалогов»):
/// текст, выбранный — зелёный с подчёркиванием; счётчик у выбранного зелёный,
/// у остальных серый. Ничего не выбрано — «Входящие»; пока выбрана папка,
/// первым стоит «← Входящие» со счётчиком инбокса; повторное касание
/// выбранного таба — тоже во «Входящие». «Спам» — таб этой полосы. Скрытая
/// папка, показанная по «Показать скрытые», — бледнее. Долгое нажатие (на
/// Маке — правый клик) — меню полосы.
private struct FolderTabs: View {
    let tabs: [FolderTab]
    let active: String?
    let inboxUnread: Int
    let menu: FolderTabMenu
    let select: (String?) -> Void

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 18) {
                if active != nil {
                    tab(icon: "", name: "← Входящие", unread: inboxUnread, on: false, hidden: false) { select(nil) }
                        .contextMenu { stripMenu }
                }
                ForEach(tabs) { t in
                    let on = t.id == active
                    tab(icon: t.icon, name: t.name, unread: t.unread, on: on, hidden: t.hidden) { select(on ? nil : t.id) }
                        .contextMenu { tabMenu(t) }
                }
                // Пустое место полосы — то же меню без пунктов папки.
                Color.clear
                    .frame(minWidth: 40, maxHeight: .infinity)
                    .contentShape(Rectangle())
                    .contextMenu { stripMenu }
                    .accessibilityElement()
                    .accessibilityLabel("Полоса папок")
                    .accessibilityIdentifier("folder.strip")
            }
            .padding(.horizontal, 14)
        }
        .frame(height: 40)
        .overlay(alignment: .bottom) { Divider() }
    }

    @ViewBuilder private func tabMenu(_ t: FolderTab) -> some View {
        Button("Все прочитаны", systemImage: "envelope.open") { menu.markRead(t.id) }
        if !t.system {
            Button("Настроить…", systemImage: "slider.horizontal.3") { menu.configure(t.id) }
            // Подтверждение — подменю: у меню нет «нажми ещё раз».
            Menu("Удалить", systemImage: "trash") {
                Button("Удалить «\(t.name)» — диалоги вернутся во «Входящие»", systemImage: "trash", role: .destructive) {
                    menu.delete(t.id)
                }
            }
        }
        Divider()
        stripMenu
    }

    @ViewBuilder private var stripMenu: some View {
        Button("Новая папка", systemImage: "folder.badge.plus", action: menu.add)
        if menu.hiddenCount > 0 {
            Button(menu.showHidden ? "Не показывать скрытые" : "Показать скрытые (\(menu.hiddenCount))",
                   systemImage: menu.showHidden ? "eye.slash" : "eye", action: menu.toggleHidden)
        }
    }

    private func tab(icon: String, name: String, unread: Int, on: Bool, hidden: Bool, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            VStack(spacing: 0) {
                Spacer(minLength: 0)
                HStack(spacing: 5) {
                    if !icon.isEmpty { Text(icon) }
                    Text(name).lineLimit(1)
                    if unread > 0 {
                        Text("\(unread)")
                            .font(.caption2.weight(.bold))
                            .foregroundStyle(.white)
                            .padding(.horizontal, 6)
                            .padding(.vertical, 1)
                            .background(Capsule().fill(on ? Palette.accent : Color.secondary.opacity(0.6)))
                    }
                }
                .font(.subheadline.weight(on ? .semibold : .regular))
                .foregroundStyle(on ? Palette.accent : Color.primary)
                .opacity(hidden && !on ? 0.5 : 1)
                Spacer(minLength: 0)
                Capsule()
                    .fill(on ? Palette.accent : Color.clear)
                    .frame(height: 2.5)
            }
            .frame(maxHeight: .infinity)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityAddTraits(on ? .isSelected : [])
        .accessibilityIdentifier("folder.tab")
    }
}

/// Строка беседы: аватар, имя, тема, дата, непрочитанное; фон — пастель
/// группы адреса получателя (§5), открытая — зелёная с белым текстом.
/// Отмеченная в режиме выбора — голубая с тёмным текстом и зелёным кружком
/// с ✓ на месте аватара, как у Android §5.
struct ConversationRow: View {
    let conversation: Conversation
    let colors: IdentityColors
    let selected: Bool
    var picked = false
    let avatars: AvatarStore?

    /// Отмеченная при выборе (Android `0xFFBFD8FF`).
    static let pickedFill = Color(red: 0xBF / 255, green: 0xD8 / 255, blue: 0xFF / 255)

    var body: some View {
        let c = conversation
        let unread = c.unreadCount > 0
        // Открытая беседа при выборе не подсвечивается: зелёное — только «открыта».
        let onSel = selected && !picked
        // Цвета текста — явные: системная подсветка выбранной строки (Мак)
        // перекрашивает `.primary`/`.secondary` в белый, а фон у строки свой —
        // белое на голубом не читалось (S-07).
        let main: Color = onSel ? .white : picked ? Self.ink : .primary
        let dim: Color = onSel ? .white.opacity(0.85) : picked ? Self.ink.opacity(0.6) : .secondary
        HStack(alignment: .top, spacing: 12) {
            if picked {
                PickedMark()
            } else {
                ConversationAvatar(name: c.label, email: c.counterparts.first?.addr ?? "", spam: c.spam, store: avatars)
            }
            VStack(alignment: .leading, spacing: 3) {
                HStack(alignment: .firstTextBaseline) {
                    // Имя — жирное всегда, у непрочитанной — ещё жирнее (десктоп §4
                    // «имя (bold) / тема», Android `SemiBold`/`Bold`, M-18).
                    Text(c.label.isEmpty ? (c.counterparts.first?.addr ?? "—") : c.label)
                        .font(.body.weight(unread ? .bold : .semibold))
                        .lineLimit(1)
                    Spacer(minLength: 8)
                    DeliveryMark(state: c.lastDelivery, selected: onSel)
                    Text(RowDate.format(c.lastDateTs))
                        .font(.footnote)
                        .foregroundStyle(dim)
                }
                HStack(alignment: .firstTextBaseline) {
                    Text(c.lastSubject.isEmpty ? "(без темы)" : c.lastSubject)
                        .font(.subheadline)
                        .foregroundStyle(onSel ? .white.opacity(0.9) : dim)
                        .lineLimit(2)
                    Spacer(minLength: 8)
                    // Спам не зазывает: числа непрочитанных нет (§5).
                    if unread && !c.spam {
                        Text("\(c.unreadCount)")
                            .font(.caption.weight(.semibold))
                            .foregroundStyle(.white)
                            .padding(.horizontal, 7)
                            .padding(.vertical, 2)
                            .background(Capsule().fill(onSel ? Color.white.opacity(0.3) : Palette.accent))
                    }
                }
            }
        }
        .foregroundStyle(main)
        .padding(.vertical, 4)
        .listRowBackground(onSel ? Palette.accent : picked ? Self.pickedFill : Color(argb: colors.color(of: c.receivedBy)))
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(picked ? .isSelected : [])
        .accessibilityIdentifier(Self.identifier(c))
    }

    /// Тёмный текст на голубом — как `onSurface` светлой темы.
    private static let ink = Color(white: 0.11)

    /// Сквозной тест IMAP сверяет беседы по id: в отладочной сборке с
    /// `-ddmail-uitest-ids` id беседы — в идентификаторе строки (озвучку не меняет).
    static func identifier(_ c: Conversation) -> String {
        #if DEBUG
        if ProcessInfo.processInfo.arguments.contains("-ddmail-uitest-ids") { return "conv.row:\(c.id)" }
        #endif
        return "conv.row"
    }
}

/// Отметка строки при выборе — на месте аватара (Android §5).
private struct PickedMark: View {
    var body: some View {
        Circle()
            .fill(Palette.accent)
            .frame(width: 40, height: 40)
            .overlay(Image(systemName: "checkmark").font(.system(size: 17, weight: .bold)).foregroundStyle(.white))
            .accessibilityHidden(true)
    }
}

/// Отметка доставки у времени строки (§5, `last_delivery_status`): последнее
/// письмо беседы — наше и не доставлено (красный значок) или ещё в очереди
/// (часы). Доставлено и неизвестно — ничего.
struct DeliveryMark: View {
    let state: DeliveryState
    let selected: Bool

    var body: some View {
        switch state {
        case .failed:
            Image(systemName: "exclamationmark.circle.fill")
                .font(.footnote)
                .foregroundStyle(selected ? Color.white : Color.red)
                .accessibilityLabel("Не доставлено")
                .accessibilityIdentifier("conv.delivery.failed")
        case .pending:
            Image(systemName: "clock")
                .font(.footnote)
                .foregroundStyle(selected ? Color.white.opacity(0.85) : Color.secondary)
                .accessibilityLabel("Отправляется")
                .accessibilityIdentifier("conv.delivery.pending")
        case .delivered, .unknown:
            EmptyView()
        }
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
    /// Строка в режиме выбора: касание (клик) отмечает её или снимает отметку,
    /// для озвучки — то же действием по умолчанию.
    @ViewBuilder func pickable(_ on: Bool, toggle: @escaping () -> Void) -> some View {
        if on {
            contentShape(Rectangle())
                .onTapGesture(perform: toggle)
                .accessibilityAction(.default, toggle)
        } else {
            self
        }
    }
}
