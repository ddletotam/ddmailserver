import DDMailAuth
import DDMailNet
import SwiftUI

/// «Настройки → Учётки» (контракт внешних учёток §1): список, повторный
/// вход, удаление (секреты и кэш учётки), добавление — то же окно, что первый запуск.
struct AccountsSection: View {
    let app: AppModel
    let workspace: Workspace
    let close: () -> Void

    var body: some View {
        let links = workspace.links
        Section {
            ForEach(links) { link in
                NavigationLink {
                    AccountDetailView(app: app, workspace: workspace, link: link, close: close)
                } label: {
                    AccountRow(link: link)
                }
                .accessibilityIdentifier("account.row")
            }
            Button("Добавить учётку", systemImage: "plus.circle") {
                close()
                app.addAccount()
            }
            .accessibilityIdentifier("account.add")
        } header: {
            Text("Учётки")
        } footer: {
            Text("Почта всех учёток — в одном списке. Пароли внешних ящиков хранятся в связке ключей этого устройства и уходят только на их серверы.")
        }
    }
}

struct AccountRow: View {
    let link: AccountLink

    var body: some View {
        let status = link.status
        VStack(alignment: .leading, spacing: 2) {
            Text(link.account.title)
            HStack(spacing: 6) {
                Text(link.account.kind == .ddmail ? "Сервер ddmail" : "IMAP · \(link.account.imap?.host ?? "")")
                switch status {
                case .dead: Text("· нужен вход").foregroundStyle(.orange)
                case let .offline(why): Text("· \(why)").lineLimit(1)
                default: EmptyView()
                }
            }
            .font(.footnote)
            .foregroundStyle(.secondary)
        }
    }
}

/// Учётка: сведения, повторный вход, удаление.
struct AccountDetailView: View {
    let app: AppModel
    let workspace: Workspace
    let link: AccountLink
    let close: () -> Void
    @State private var confirmRemove = false
    @State private var name = ""
    @State private var extra = ""
    @State private var caldav = ""
    @State private var carddav = ""
    @State private var loaded = false

    var body: some View {
        let a = link.account
        Form {
            Section {
                LabeledContent("Адрес", value: a.email.isEmpty ? "—" : a.email)
                LabeledContent("Ключ учётки", value: a.key)
                if a.kind == .ddmail {
                    LabeledContent("Сервер", value: a.serverURL)
                } else {
                    if let i = a.imap { LabeledContent("IMAP", value: "\(i.host):\(i.port)") }
                    if let s = a.smtp { LabeledContent("SMTP", value: "\(s.host):\(s.port)") }
                }
            }
            if a.kind == .imap {
                Section {
                    TextField("Имя в письмах", text: $name, prompt: Text("Как вас подписывать"))
                    TextField("Другие мои адреса", text: $extra, prompt: Text("через запятую"), axis: .vertical)
                        .autocorrectionDisabled()
                } footer: {
                    Text("Адреса, с которых вы тоже пишете через этот ящик (псевдонимы). По ним беседы понимают, где вы, а где собеседник.")
                }
                Section {
                    TextField("CalDAV", text: $caldav, prompt: Text("https://… (календарь)"))
                        .autocorrectionDisabled()
                        .accessibilityIdentifier("account.caldav")
                    TextField("CardDAV", text: $carddav, prompt: Text("https://… (контакты)"))
                        .autocorrectionDisabled()
                        .accessibilityIdentifier("account.carddav")
                } header: {
                    Text("Календарь и контакты")
                } footer: {
                    Text("Находятся по домену при подключении; вход — тем же паролем. Пусто — календаря и контактов этой учётки нет.")
                }
                Section {
                    Button("Сохранить", systemImage: "checkmark") {
                        var b = a
                        b.displayName = name.trimmingCharacters(in: .whitespaces)
                        b.extraAddresses = extra.split(whereSeparator: { ",; \n".contains($0) })
                            .map { $0.trimmingCharacters(in: .whitespaces).lowercased() }.filter { $0.contains("@") }
                        b.caldavURL = caldav.trimmingCharacters(in: .whitespaces)
                        b.carddavURL = carddav.trimmingCharacters(in: .whitespaces)
                        close()
                        app.update(b)
                    }
                    .disabled(name == a.displayName && extra == a.extraAddresses.joined(separator: ", ")
                              && caldav == a.caldavURL && carddav == a.carddavURL)
                }
            }
            Section {
                Button("Войти заново", systemImage: "person.badge.key") {
                    close()
                    app.relogin(a.key)
                }
                Button("Удалить учётку", systemImage: "trash", role: .destructive) { confirmRemove = true }
                    .accessibilityIdentifier("account.remove")
            }
        }
        .formStyle(.grouped)
        .navigationTitle(a.title)
        .onAppear {
            guard !loaded else { return }
            loaded = true
            name = a.displayName
            extra = a.extraAddresses.joined(separator: ", ")
            caldav = a.caldavURL
            carddav = a.carddavURL
        }
        .confirmationDialog("Удалить учётку \(a.title)?", isPresented: $confirmRemove, titleVisibility: .visible) {
            Button("Удалить", role: .destructive) {
                close()
                app.remove(a.key)
            }
        } message: {
            Text("Пароль или сессия и почта этой учётки будут удалены с устройства. На сервере ничего не изменится.")
        }
    }
}
