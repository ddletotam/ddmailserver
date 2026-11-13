import DDMailSync
import SwiftUI

/// «Настройки → Папки» (десктоп §5г, вкладка «Папки»): выключатель
/// «Показывать папки» этого устройства и список папок — порядок (он же
/// приоритет правил), удаление, настройка каждой. Сами папки общие: хранит
/// их сервер ddmail, без ddmail-учётки — устройство. Кнопок «Сохранить» нет:
/// всё сохраняется сразу.
struct FoldersSettingsView: View {
    @Bindable var mail: MailModel
    let workspace: Workspace
    @State private var opened: FolderRef?
    @State private var confirmDelete: DialogFolder?

    var body: some View {
        // Наблюдаемое — в теле (контракт §0).
        let folders = mail.doc.userFolders
        let shared = mail.foldersShared
        Form {
            Section {
                Toggle("Показывать папки", isOn: $mail.foldersOn)
                    .accessibilityIdentifier("folders.on")
            } footer: {
                Text("Табы папок над списком бесед на этом устройстве. Выключено — список как без папок.")
            }
            Section {
                ForEach(folders, id: \.id) { f in
                    NavigationLink(value: FolderRef(id: f.id)) {
                        FolderRow(folder: f)
                    }
                    .swipeActions {
                        Button("Удалить", systemImage: "trash", role: .destructive) { confirmDelete = f }
                    }
                    .contextMenu {
                        Button("Удалить…", systemImage: "trash", role: .destructive) { confirmDelete = f }
                    }
                }
                .onMove { mail.moveFolders(from: $0, to: $1) }
                Button("Новая папка", systemImage: "folder.badge.plus") {
                    Task { if let id = await mail.addFolder() { opened = FolderRef(id: id) } }
                }
                .accessibilityIdentifier("folders.add")
            } header: {
                Text("Папки")
            } footer: {
                Text(shared
                     ? "Папки общие для всех устройств и десктопа — хранит их сервер. Порядок — он же приоритет: диалог попадает в первую папку, чьи правила подходят. Всё сохраняется сразу."
                     : "Папки хранятся на этом устройстве. Порядок — он же приоритет: диалог попадает в первую папку, чьи правила подходят. Всё сохраняется сразу.")
            }
        }
        .formStyle(.grouped)
        .navigationTitle("Папки")
        .toolbarTitleDisplayMode(.inline)
        #if os(iOS)
        .toolbar {
            if !folders.isEmpty { ToolbarItem(placement: .primaryAction) { EditButton() } }
        }
        #endif
        .navigationDestination(item: $opened) { f in
            FolderEditorView(mail: mail, workspace: workspace, folderID: f.id)
        }
        .navigationDestination(for: FolderRef.self) { f in
            FolderEditorView(mail: mail, workspace: workspace, folderID: f.id)
        }
        .confirmationDialog(deleteTitle, isPresented: Binding(get: { confirmDelete != nil }, set: { if !$0 { confirmDelete = nil } }),
                            titleVisibility: .visible) {
            Button("Удалить", role: .destructive) { if let f = confirmDelete { mail.deleteFolder(f.id) } }
        } message: {
            Text("Диалоги, положенные в неё руками, вернутся во «Входящие».")
        }
    }

    private var deleteTitle: String {
        let name = confirmDelete?.name ?? ""
        return mail.foldersShared ? "Удалить папку «\(name)» на всех устройствах?" : "Удалить папку «\(name)»?"
    }
}

/// Строка папки в списке настроек: значок, имя, кратко правила.
private struct FolderRow: View {
    let folder: DialogFolder

    var body: some View {
        HStack(spacing: 10) {
            Text(folder.icon.isEmpty ? "📁" : folder.icon)
            VStack(alignment: .leading, spacing: 2) {
                Text(folder.name.isEmpty ? "Без имени" : folder.name)
                let s = summary
                if !s.isEmpty { Text(s).font(.caption).foregroundStyle(.secondary).lineLimit(1) }
            }
            if folder.hidden {
                Spacer()
                Image(systemName: "eye.slash").foregroundStyle(.secondary).accessibilityLabel("Не показывается в списке")
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier("folders.row")
    }

    private var summary: String {
        var parts: [String] = []
        if !folder.rules.identities.isEmpty { parts.append("на " + folder.rules.identities.joined(separator: ", ")) }
        if !folder.rules.domains.isEmpty { parts.append(folder.rules.domains.joined(separator: ", ")) }
        switch folder.rules.kind {
        case .direct: parts.append("1:1")
        case .group: parts.append("группы")
        case .any: break
        }
        if !folder.include.isEmpty { parts.append("вручную: \(folder.include.count)") }
        return parts.joined(separator: " · ")
    }
}

/// Настройка одной папки (десктоп §4, `folder_edit`): имя и значок; правила
/// «Получено на», «Домены», «Диалоги: все / 1:1 / группы» (между
/// категориями «И», внутри списка «ИЛИ»); «Не показывать в списке».
/// Поля правятся как обычный текст и уходят с паузой 800 мс (как дебаунс
/// десктопа) — документ не пересоздаёт поля под пальцами.
struct FolderEditorView: View {
    let mail: MailModel
    let workspace: Workspace
    let folderID: String
    @State private var edit: Fields?
    @State private var saved: Fields?
    @State private var confirmDelete = false
    @Environment(\.dismiss) private var dismiss

    /// Редактируемое: всё, кроме ручных назначений.
    struct Fields: Equatable {
        var name: String
        var icon: String
        var identities: [String]
        var domains: String
        var kind: DialogKind
        var hidden: Bool

        init(_ f: DialogFolder) {
            name = f.name
            icon = f.icon
            identities = f.rules.identities
            domains = f.rules.domains.joined(separator: ", ")
            kind = f.rules.kind
            hidden = f.hidden
        }
    }

    var body: some View {
        let folder = mail.doc.folders.first { $0.id == folderID }
        Group {
            if let folder, !folder.isSystem, edit != nil {
                form(folder)
            } else if folder == nil {
                ContentUnavailableView("Папки больше нет", systemImage: "folder.badge.minus")
            } else {
                ProgressView()
            }
        }
        .navigationTitle(edit.map { $0.name.isEmpty ? "Папка" : $0.name } ?? "Папка")
        .toolbarTitleDisplayMode(.inline)
        .onAppear {
            if edit == nil, let folder {
                edit = Fields(folder)
                saved = edit
            }
        }
        // Пауза после последней правки — и в документ (на сервер — следом).
        .task(id: edit) {
            guard edit != saved else { return }
            try? await Task.sleep(for: .milliseconds(800))
            guard !Task.isCancelled else { return }
            save()
        }
        .onDisappear(perform: save)
    }

    private func form(_ folder: DialogFolder) -> some View {
        let bind = Binding(get: { edit! }, set: { edit = $0 })
        let own = ownAddresses(plus: edit?.identities ?? [])
        return Form {
            Section {
                TextField("Название", text: bind.name)
                    .accessibilityIdentifier("folder.name")
                TextField("Значок (эмодзи)", text: bind.icon)
                    .onChange(of: edit?.icon ?? "") { _, v in
                        let c = clipFolderIcon(v)
                        if c != v { edit?.icon = c }
                    }
            }
            Section {
                ForEach(own, id: \.self) { addr in
                    Toggle(addr, isOn: Binding(
                        get: { edit?.identities.contains { $0.caseInsensitiveCompare(addr) == .orderedSame } ?? false },
                        set: { on in
                            edit?.identities.removeAll { $0.caseInsensitiveCompare(addr) == .orderedSame }
                            if on { edit?.identities.append(addr) }
                        }
                    ))
                }
            } header: {
                Text("Получено на")
            } footer: {
                Text("Свои адреса, на которые пришла переписка. Ничего не выбрано — любой.")
            }
            Section {
                TextField("acme.com, example.org", text: bind.domains, axis: .vertical)
                    .autocorrectionDisabled()
                    #if os(iOS)
                    .textInputAutocapitalization(.never)
                    .keyboardType(.URL)
                    #endif
                    .accessibilityIdentifier("folder.domains")
            } header: {
                Text("Домены")
            } footer: {
                Text("Домен собеседника, через запятую; acme.com ловит и mail.acme.com.")
            }
            Section {
                Picker("Диалоги", selection: bind.kind) {
                    Text("Все").tag(DialogKind.any)
                    Text("1:1").tag(DialogKind.direct)
                    Text("Группы").tag(DialogKind.group)
                }
                .pickerStyle(.segmented)
            } header: {
                Text("Диалоги")
            } footer: {
                Text("Между правилами — «и», внутри списка — «или». Папка без правил набирается только руками: «В папку…» в меню беседы.")
            }
            Section {
                Toggle("Не показывать в списке", isOn: bind.hidden)
                    .accessibilityIdentifier("folder.hidden")
            } footer: {
                Text("Таба нет, диалоги папки не видны в списке и приходят без уведомлений. Вернуть — долгое нажатие на табы → «Показать скрытые».")
            }
            if !folder.include.isEmpty || !folder.exclude.isEmpty {
                Section {
                    Button("Забыть ручные назначения (\(folder.include.count))", systemImage: "arrow.uturn.backward") {
                        mail.updateFolder(folderID) { $0.include = []; $0.exclude = [] }
                    }
                } footer: {
                    Text("Диалоги, положенные в папку руками, и исключённые из неё — снова по правилам.")
                }
            }
            Section {
                Button("Удалить папку", systemImage: "trash", role: .destructive) { confirmDelete = true }
            }
        }
        .formStyle(.grouped)
        .confirmationDialog(mail.foldersShared ? "Удалить папку «\(folder.name)» на всех устройствах?" : "Удалить папку «\(folder.name)»?",
                            isPresented: $confirmDelete, titleVisibility: .visible) {
            Button("Удалить", role: .destructive) {
                saved = edit
                mail.deleteFolder(folderID)
                dismiss()
            }
        } message: {
            Text("Диалоги, положенные в неё руками, вернутся во «Входящие».")
        }
    }

    /// Свои адреса всех учёток и уже выбранные (вдруг адреса больше нет).
    private func ownAddresses(plus chosen: [String]) -> [String] {
        var seen = Set<String>()
        let all = workspace.links.flatMap { $0.provider.identities().map(\.email) } + chosen
        return all.filter { seen.insert($0.lowercased()).inserted }
    }

    private func save() {
        guard let e = edit, e != saved else { return }
        saved = e
        let name = e.name.trimmingCharacters(in: .whitespaces)
        let icon = clipFolderIcon(e.icon)
        let rules = FolderRules(identities: e.identities.map { $0.lowercased() }, domains: splitFolderList(e.domains), kind: e.kind)
        let hidden = e.hidden
        mail.updateFolder(folderID) { f in
            f.name = name
            f.icon = icon
            f.rules = rules
            f.hidden = hidden
        }
    }
}
