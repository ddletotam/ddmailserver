import SwiftUI

/// Подключение (контракт §3): системная форма; на iPad и Маке колонка
/// ограничена по ширине — поля во всю ширину большого экрана читаются плохо.
struct ConnectView: View {
    @Bindable var model: ConnectModel
    @FocusState private var focus: Field?

    enum Field { case email, server, username, password }

    var body: some View {
        NavigationStack {
            Form {
                switch model.step {
                case .email: emailStep
                case .searching: searchingStep
                case .found, .manual: loginStep
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
            .navigationTitle(model.title)
            .toolbar {
                if model.step != .email {
                    ToolbarItem(placement: .cancellationAction) {
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
            Button("Ввести адрес сервера вручную", action: model.manual)
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
        Section {
            Button(action: model.login) {
                HStack {
                    Text(model.busy ? "Входим…" : "Войти")
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
