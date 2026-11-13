import DDMailNet
import SwiftUI

/// Подключение (контракт §3): системная форма; на iPad и Маке колонка
/// ограничена по ширине — поля во всю ширину большого экрана читаются плохо.
struct ConnectView: View {
    @Bindable var model: ConnectModel
    /// Окно поверх рабочего пространства — его можно закрыть.
    var onCancel: (() -> Void)? = nil
    @FocusState private var focus: Field?

    enum Field { case email, server, username, password, imapHost, login }

    var body: some View {
        NavigationStack {
            Form {
                switch model.step {
                case .email: emailStep
                case .searching: searchingStep
                case .found, .manual: loginStep
                case .imap: imapStep
                case .imapManual: imapManualStep
                }
                if let notice = model.notice {
                    Section {
                        Label(notice, systemImage: "info.circle")
                            .foregroundStyle(.secondary)
                    }
                }
                if let error = model.error {
                    Section {
                        Label(error, systemImage: "exclamationmark.triangle")
                            .foregroundStyle(.red)
                    }
                }
            }
            .formStyle(.grouped)
            .frame(maxWidth: 560)
            .frame(maxWidth: .infinity)
            #if os(iOS)
            // Фон формы — на всё окно, а не только в колонке 560 pt (iPad).
            .background(Color(uiColor: .systemGroupedBackground))
            #endif
            .navigationTitle(model.title)
            .toolbar {
                if let onCancel {
                    ToolbarItem(placement: .cancellationAction) {
                        Button("Закрыть", systemImage: "xmark", action: onCancel)
                            .disabled(model.busy)
                    }
                }
                if model.canGoBack {
                    ToolbarItem(placement: .navigation) {
                        Button("Назад", systemImage: "chevron.backward") { model.back() }
                            .disabled(model.busy)
                    }
                }
            }
            .animation(.default, value: model.step)
        }
        .onChange(of: model.step, initial: true) { _, step in
            switch step {
            case .email: focus = .email
            // Нашли сервер и подставили логин — осталось ввести пароль.
            case .found: focus = model.username.isEmpty ? .username : .password
            case .manual: focus = .server
            case .searching: focus = nil
            case .imap: focus = model.imapUser.isEmpty ? .login : .password
            case .imapManual: focus = model.imapHost.isEmpty ? .imapHost : nil
            }
        }
    }

    @ViewBuilder private var emailStep: some View {
        Section {
            TextField("Адрес почты", text: $model.email, prompt: Text("имя@домен"))
                .emailInput()
                .accessibilityIdentifier("connect.email")
                .focused($focus, equals: .email)
                .submitLabel(.search)
                .onSubmit(model.findServer)
        } footer: {
            Text("Введите адрес почты — сервер найдём сами.")
        }
        Section {
            Button("Найти сервер", action: model.findServer)
                .accessibilityIdentifier("connect.find")
                .disabled(model.email.trimmingCharacters(in: .whitespaces).isEmpty)
            Button("Я знаю все настройки", action: model.imapManual)
                .accessibilityIdentifier("connect.manualIMAP")
            Button("Ввести адрес сервера ddmail вручную", action: model.manual)
        }
    }

    @ViewBuilder private var searchingStep: some View {
        Section {
            VStack(alignment: .leading, spacing: 10) {
                Text(model.progress?.stage ?? model.email)
                ProgressView(
                    value: Double(model.progress?.done ?? 0),
                    total: Double(model.progress?.total ?? 1)
                )
                if let p = model.progress {
                    Text("Проверено \(p.done) из \(p.total)")
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                }
            }
            .padding(.vertical, 4)
            .animation(.default, value: model.progress?.done)
        }
    }

    @ViewBuilder private var loginStep: some View {
        Section {
            TextField("Сервер", text: $model.server, prompt: Text("mail.example.ru"))
                .urlInput()
                .accessibilityIdentifier("connect.server")
                .focused($focus, equals: .server)
                .disabled(model.step == .found || model.busy)
                .submitLabel(.next)
                .onSubmit { focus = .username }
            TextField("Логин", text: $model.username)
                .loginInput()
                .accessibilityIdentifier("connect.username")
                .focused($focus, equals: .username)
                .disabled(model.busy)
                .submitLabel(.next)
                .onSubmit { focus = .password }
            SecureField("Пароль", text: $model.password)
                .textContentType(.password)
                .accessibilityIdentifier("connect.password")
                .focused($focus, equals: .password)
                .disabled(model.busy)
                .submitLabel(.go)
                .onSubmit(model.login)
        } footer: {
            Text("Логин — имя пользователя на сервере, обычно часть адреса до @. Пароль не сохраняется: устройство получает от сервера токен сессии.")
        }
        loginButton
    }

    @ViewBuilder private var loginButton: some View {
        Section {
            Button(action: model.login) {
                HStack {
                    Text(model.busy ? model.busyText : "Войти")
                    if model.busy {
                        Spacer()
                        ProgressView()
                    }
                }
            }
            .disabled(model.busy)
            .accessibilityIdentifier("connect.login")
        }
    }

    // MARK: IMAP

    /// Серверы найдены: осталось ввести пароль (логин подставлен).
    @ViewBuilder private var imapStep: some View {
        Section {
            LabeledContent("Адрес", value: model.email)
            LabeledContent("Входящая (IMAP)", value: "\(model.imapHost):\(model.imapPort)")
            LabeledContent("Исходящая (SMTP)", value: "\(model.smtpHost):\(model.smtpPort)" + (model.smtpNoAuth ? ", без авторизации" : ""))
            Button("Изменить настройки", systemImage: "slider.horizontal.3", action: model.imapManual)
                .disabled(model.busy)
        }
        Section {
            TextField("Логин", text: $model.imapUser)
                .loginInput()
                .accessibilityIdentifier("connect.login.name")
                .focused($focus, equals: .login)
                .disabled(model.busy)
                .submitLabel(.next)
                .onSubmit { focus = .password }
            passwordField
        } footer: {
            Text(model.passwordHint ?? "Пароль хранится в связке ключей этого устройства и уходит только на серверы этого ящика.")
        }
        loginButton
    }

    private var passwordField: some View {
        SecureField("Пароль", text: $model.password)
            .textContentType(.password)
            .accessibilityIdentifier("connect.password")
            .focused($focus, equals: .password)
            .disabled(model.busy)
            .submitLabel(.go)
            .onSubmit(model.login)
    }

    /// «Я знаю все настройки» — ручная форма.
    @ViewBuilder private var imapManualStep: some View {
        Section {
            TextField("Адрес почты", text: $model.email, prompt: Text("имя@домен"))
                .emailInput()
                .accessibilityIdentifier("connect.manual.email")
                .disabled(model.busy || model.isRelogin)
            TextField("Имя в письмах", text: $model.displayName, prompt: Text("Как вас подписывать"))
                .disabled(model.busy)
        }
        Section("Входящая почта (IMAP)") {
            TextField("Сервер", text: $model.imapHost, prompt: Text("imap.example.ru"))
                .urlInput()
                .accessibilityIdentifier("connect.imap.host")
                .focused($focus, equals: .imapHost)
            TextField("Порт", text: $model.imapPort)
                .numberInput()
                .accessibilityIdentifier("connect.imap.port")
                .onChange(of: model.imapPort) { model.imapPortChanged() }
            SecurityPicker(selection: $model.imapSecurity)
        }
        .disabled(model.busy)
        Section("Исходящая почта (SMTP)") {
            TextField("Сервер", text: $model.smtpHost, prompt: Text("smtp.example.ru"))
                .urlInput()
                .accessibilityIdentifier("connect.smtp.host")
            TextField("Порт", text: $model.smtpPort)
                .numberInput()
                .accessibilityIdentifier("connect.smtp.port")
                .onChange(of: model.smtpPort) { model.smtpPortChanged() }
            SecurityPicker(selection: $model.smtpSecurity)
            Toggle("SMTP без авторизации", isOn: $model.smtpNoAuth)
        }
        .disabled(model.busy)
        Section {
            TextField("Логин", text: $model.imapUser)
                .loginInput()
                .accessibilityIdentifier("connect.login.name")
                .focused($focus, equals: .login)
                .disabled(model.busy)
            passwordField
        } header: {
            Text("Вход")
        } footer: {
            Text("Открытым текстом клиент не соединяется: 993 и 465 — TLS сразу, 143 и 587 — STARTTLS. Перед сохранением проверяется вход на оба сервера.")
        }
        loginButton
    }
}

/// TLS или STARTTLS; открытого текста нет (контракт внешних учёток §10).
private struct SecurityPicker: View {
    @Binding var selection: MailSecurity

    var body: some View {
        Picker("Шифрование", selection: $selection) {
            Text("TLS").tag(MailSecurity.tls)
            Text("STARTTLS").tag(MailSecurity.startTLS)
        }
    }
}

private extension View {
    /// Поле адреса почты: без автозамены и заглавных, клавиатура с @.
    func emailInput() -> some View {
        #if os(iOS)
        textContentType(.emailAddress)
            .keyboardType(.emailAddress)
            .textInputAutocapitalization(.never)
            .autocorrectionDisabled()
        #else
        textContentType(.emailAddress).autocorrectionDisabled()
        #endif
    }

    func urlInput() -> some View {
        #if os(iOS)
        textContentType(.URL)
            .keyboardType(.URL)
            .textInputAutocapitalization(.never)
            .autocorrectionDisabled()
        #else
        textContentType(.URL).autocorrectionDisabled()
        #endif
    }

    func numberInput() -> some View {
        #if os(iOS)
        keyboardType(.numberPad)
        #else
        self
        #endif
    }

    /// Логин — `.username`: связка ключей iCloud подставит пару логин/пароль.
    func loginInput() -> some View {
        #if os(iOS)
        textContentType(.username)
            .textInputAutocapitalization(.never)
            .autocorrectionDisabled()
        #else
        textContentType(.username).autocorrectionDisabled()
        #endif
    }
}
