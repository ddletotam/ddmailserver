import DDMailAPI
import DDMailDAV
import SwiftUI

/// Контакты (Android §15): одна сводная книга, как её отдаёт сервер; поиск
/// локальный — имя, организация, адреса, цифры телефона.
@MainActor @Observable
final class ContactsModel {
    /// ddmail и CardDAV внешних учёток вместе (внешние учётки §8).
    let api: any ContactsSource
    private let onDead: () -> Void
    private(set) var contacts: [ContactDTO] = []
    private(set) var books: [AddressBookDTO] = []
    private(set) var loaded = false
    var query = ""
    var selectedID: Int64?
    var form: ContactForm?
    var notice: String?
    let prefs = CalendarPrefs.shared

    struct ContactForm: Equatable {
        var editing: Int64?
        var name = ""
        var organization = ""
        var title = ""
        var emails = [""]
        var phones = [""]
        var bookID: Int64 = 0
        var error: String?
        var busy = false
    }

    init(api: any ContactsSource, onDead: @escaping () -> Void) {
        self.api = api
        self.onDead = onDead
    }

    func load() async {
        do {
            async let c = api.contacts(limit: 5000)
            async let b = api.addressBooks()
            contacts = try await c.sorted { $0.fullName.localizedStandardCompare($1.fullName) == .orderedAscending }
            books = (try? await b) ?? books
            loaded = true
        } catch APIError.unauthorized {
            onDead()
        } catch is CancellationError {
        } catch {
            notice = error.localizedDescription
        }
    }

    var writableBooks: [AddressBookDTO] { books.filter(\.canWrite) }

    var filtered: [ContactDTO] {
        let q = query.trimmingCharacters(in: .whitespaces)
        guard !q.isEmpty else { return contacts }
        let digits = q.filter(\.isNumber)
        return contacts.filter { c in
            c.fullName.localizedStandardContains(q) || c.organization.localizedStandardContains(q)
                || c.emails.contains { $0.localizedStandardContains(q) }
                || (!digits.isEmpty && c.phones.contains { $0.filter(\.isNumber).contains(digits) })
        }
    }

    /// Буквенные разделы списка.
    var sections: [(letter: String, items: [ContactDTO])] {
        let groups = Dictionary(grouping: filtered) { c -> String in
            let first = c.fullName.trimmingCharacters(in: .whitespaces).first.map { String($0).uppercased() } ?? "#"
            return first.first?.isLetter == true ? first : "#"
        }
        return groups.keys.sorted { a, b in
            if a == "#" { return false }
            if b == "#" { return true }
            return a.localizedStandardCompare(b) == .orderedAscending
        }.map { ($0, groups[$0] ?? []) }
    }

    var selected: ContactDTO? { selectedID.flatMap { id in contacts.first { $0.id == id } } }

    func newContact() {
        // Только назначенная книга; иначе выбор в форме обязателен.
        let book = writableBooks.first { $0.id == prefs.defaultAddressBook }?.id ?? 0
        form = ContactForm(bookID: book, error: writableBooks.isEmpty ? "Нет адресных книг, доступных на запись" : nil)
    }

    func edit(_ c: ContactDTO) {
        form = ContactForm(
            editing: c.id, name: c.fullName, organization: c.organization, title: c.title,
            emails: c.emails.isEmpty ? [""] : Array(c.emails.prefix(3)), phones: c.phones.isEmpty ? [""] : Array(c.phones.prefix(3))
        )
    }

    func save() {
        guard var f = form, !f.busy else { return }
        let name = f.name.trimmingCharacters(in: .whitespaces)
        let emails = f.emails.map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
        let phones = f.phones.map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
        if name.isEmpty && emails.isEmpty { form?.error = "Нужно имя или адрес"; return }
        if f.editing == nil && f.bookID == 0 { form?.error = "Выберите адресную книгу"; return }
        f.busy = true
        f.error = nil
        form = f
        let req = ContactWriteRequest(fullName: name, emails: emails, phones: phones, organization: f.organization,
                                      title: f.title, addressBookID: f.editing == nil ? f.bookID : nil)
        Task {
            do {
                if let id = f.editing { try await api.patchContact(id: id, req) } else { try await api.createContact(req) }
                form = nil
                notice = f.editing == nil ? "Контакт создан" : "Сохранено"
                await load()
            } catch {
                if case APIError.unauthorized = error { onDead() }
                // Книга только для чтения — 403 текстом, не выход из сессии (§2).
                form?.busy = false
                form?.error = error.localizedDescription
            }
        }
    }

    func delete(_ c: ContactDTO) {
        Task {
            do {
                try await api.deleteContact(id: c.id)
                selectedID = nil
                notice = "Контакт удалён"
                await load()
            } catch {
                if case APIError.unauthorized = error { onDead() }
                notice = error.localizedDescription
            }
        }
    }
}

struct ContactsView: View {
    @Bindable var model: ContactsModel
    let avatars: AvatarStore?
    /// «Написать» — новое письмо на адрес (переход в почту).
    let write: (String) -> Void

    var body: some View {
        let formOpen = model.form != nil
        return NavigationSplitView {
            let sections = model.sections
            List(selection: $model.selectedID) {
                ForEach(sections, id: \.letter) { s in
                    Section(s.letter) {
                        ForEach(s.items) { c in
                            HStack(spacing: 12) {
                                ConversationAvatar(name: c.fullName, email: c.emails.first ?? "", spam: false, store: avatars)
                                VStack(alignment: .leading) {
                                    Text(c.fullName.isEmpty ? (c.emails.first ?? "—") : c.fullName)
                                    if !c.organization.isEmpty { Text(c.organization).font(.caption).foregroundStyle(.secondary) }
                                }
                            }
                            .tag(c.id)
                        }
                    }
                }
            }
            .overlay {
                if !model.loaded { ProgressView() } else if model.filtered.isEmpty {
                    if model.query.isEmpty { ContentUnavailableView("Контактов нет", systemImage: "person.crop.circle") }
                    else { ContentUnavailableView.search(text: model.query) }
                }
            }
            .navigationSplitViewColumnWidth(min: 280, ideal: 340, max: 460)
            .searchable(text: $model.query, prompt: "Имя, адрес, телефон")
            .refreshable { await model.load() }
            .navigationTitle("Контакты")
            .toolbar {
                ToolbarItem(placement: .primaryAction) {
                    Button("Новый контакт", systemImage: "plus") { model.newContact() }
                }
            }
        } detail: {
            if let c = model.selected {
                ContactCard(model: model, contact: c, avatars: avatars, write: write)
            } else {
                ContentUnavailableView("Контакт не выбран", systemImage: "person.crop.circle")
            }
        }
        .tint(Palette.accent)
        .sheet(isPresented: Binding(get: { formOpen }, set: { if !$0 { model.form = nil } })) {
            ContactFormView(model: model)
        }
        .overlay(alignment: .bottom) { NoticeBanner(text: $model.notice) }
        .task { await model.load() }
    }
}

private struct ContactCard: View {
    let model: ContactsModel
    let contact: ContactDTO
    let avatars: AvatarStore?
    let write: (String) -> Void
    @Environment(\.openURL) private var openURL
    @State private var confirmDelete = false

    var body: some View {
        List {
            Section {
                VStack(spacing: 8) {
                    ConversationAvatar(name: contact.fullName, email: contact.emails.first ?? "", spam: false, store: avatars)
                        .scaleEffect(1.8)
                        .frame(height: 80)
                    Text(contact.fullName).font(.title2.bold())
                    let sub = [contact.title, contact.organization].filter { !$0.isEmpty }.joined(separator: " · ")
                    if !sub.isEmpty { Text(sub).foregroundStyle(.secondary) }
                }
                .frame(maxWidth: .infinity)
                .listRowBackground(Color.clear)
            }
            if !contact.emails.isEmpty {
                Section("Почта") {
                    ForEach(contact.emails, id: \.self) { e in
                        Button { write(e) } label: { Label(e, systemImage: "envelope") }
                            .contextMenu { Button("Скопировать", systemImage: "doc.on.doc") { Clipboard.copy(e) } }
                    }
                }
            }
            if !contact.phones.isEmpty {
                Section("Телефон") {
                    ForEach(contact.phones, id: \.self) { p in
                        Button {
                            if let u = URL(string: "tel:" + p.filter { $0.isNumber || $0 == "+" }) { openURL(u) }
                        } label: { Label(p, systemImage: "phone") }
                            .contextMenu { Button("Скопировать", systemImage: "doc.on.doc") { Clipboard.copy(p) } }
                    }
                }
            }
            Section {
                Button("Удалить контакт", systemImage: "trash", role: .destructive) { confirmDelete = true }
            }
        }
        .navigationTitle(contact.fullName)
        .toolbarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .primaryAction) { Button("Изменить", systemImage: "pencil") { model.edit(contact) } }
        }
        .confirmationDialog("Удалить контакт?", isPresented: $confirmDelete, titleVisibility: .visible) {
            Button("Удалить", role: .destructive) { model.delete(contact) }
        }
    }
}

/// Форма контакта: до трёх адресов и телефонов (столько хранит сервер); книга
/// нового контакта — только назначенная пользователем (§15).
private struct ContactFormView: View {
    @Bindable var model: ContactsModel

    var body: some View {
        if let f = model.form {
            NavigationStack {
                Form {
                    Section {
                        TextField("Имя", text: bind(\.name))
                        TextField("Организация", text: bind(\.organization))
                        TextField("Должность", text: bind(\.title))
                    }
                    Section("Почта") {
                        ForEach(f.emails.indices, id: \.self) { i in
                            TextField("адрес@домен", text: Binding(get: { model.form?.emails[safe: i] ?? "" }, set: { model.form?.emails[i] = $0 }))
                                #if os(iOS)
                                .keyboardType(.emailAddress).textInputAutocapitalization(.never)
                                #endif
                                .autocorrectionDisabled()
                        }
                        if f.emails.count < 3 { Button("Ещё адрес", systemImage: "plus") { model.form?.emails.append("") } }
                    }
                    Section("Телефон") {
                        ForEach(f.phones.indices, id: \.self) { i in
                            TextField("+7 …", text: Binding(get: { model.form?.phones[safe: i] ?? "" }, set: { model.form?.phones[i] = $0 }))
                                #if os(iOS)
                                .keyboardType(.phonePad)
                                #endif
                        }
                        if f.phones.count < 3 { Button("Ещё телефон", systemImage: "plus") { model.form?.phones.append("") } }
                    }
                    if f.editing == nil {
                        Section {
                            Picker("Адресная книга", selection: bind(\.bookID)) {
                                if f.bookID == 0 { Text("Выберите").tag(Int64(0)) }
                                ForEach(model.writableBooks) { b in
                                    Text(b.sourceName.isEmpty ? b.name : "\(b.name) · \(b.sourceName)").tag(b.id)
                                }
                            }
                            if f.bookID != 0 {
                                Toggle("Для новых контактов", isOn: Binding(
                                    get: { model.prefs.defaultAddressBook == f.bookID },
                                    set: { model.prefs.defaultAddressBook = $0 ? f.bookID : nil }
                                ))
                            }
                        }
                    }
                    if let e = f.error {
                        Section { Label(e, systemImage: "exclamationmark.triangle").foregroundStyle(.red) }
                    }
                }
                .formStyle(.grouped)
                .navigationTitle(f.editing == nil ? "Новый контакт" : "Изменить контакт")
                .toolbarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) { Button("Отмена", systemImage: "xmark") { model.form = nil }.disabled(f.busy) }
                    ToolbarItem(placement: .confirmationAction) {
                        if f.busy { ProgressView() } else { Button("Сохранить", systemImage: "checkmark") { model.save() } }
                    }
                }
            }
            .interactiveDismissDisabled(f.busy)
        }
    }

    private func bind<T>(_ kp: WritableKeyPath<ContactsModel.ContactForm, T>) -> Binding<T> {
        Binding(
            get: { model.form?[keyPath: kp] ?? ContactsModel.ContactForm()[keyPath: kp] },
            set: {
                model.form?[keyPath: kp] = $0
                model.form?.error = nil
            }
        )
    }
}

extension Array {
    subscript(safe i: Int) -> Element? { indices.contains(i) ? self[i] : nil }
}
