import DDMailAPI
import DDMailSync
import PhotosUI
import SwiftUI
import UniformTypeIdentifiers

/// Строка ответа под лентой (§5, §9): текст, вложения, отправка кнопкой.
/// На iPhone Enter — новая строка; на Маке Enter — отправить, Shift+Enter —
/// новая строка (как десктоп); с клавиатурой у iPad — Cmd+Enter.
struct ComposerBar: View {
    @Bindable var model: ComposeModel
    /// Фокус в строку сразу (новая беседа: адрес выбран — пишем).
    var autofocus = false
    /// Ответ в беседе или первое письмо новой беседы.
    let onSend: () -> Void
    @State private var importing = false
    @State private var photos: [PhotosPickerItem] = []
    @FocusState private var focused: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            if let r = model.replyTo {
                HStack(spacing: 6) {
                    Image(systemName: "arrowshape.turn.up.left").foregroundStyle(Palette.accent)
                    Text("Ответ на «\(r.subject.isEmpty ? "(без темы)" : r.subject)»").lineLimit(1)
                    Spacer()
                    Button("Отменить ответ", systemImage: "xmark.circle.fill") { model.replyTo = nil }
                        .labelStyle(.iconOnly)
                        .foregroundStyle(.secondary)
                }
                .font(.footnote)
            }
            if model.expanded { OverrideFields(draft: $model.draft, identities: model.identities) }
            AttachmentChips(attachments: $model.draft.attachments)
            if let e = model.error {
                Label(e, systemImage: "exclamationmark.triangle").font(.footnote).foregroundStyle(.red)
            }
            HStack(alignment: .bottom, spacing: 8) {
                Menu {
                    Button("Файл", systemImage: "doc") { importing = true }
                    PhotosPicker(selection: $photos, matching: .any(of: [.images, .videos])) {
                        Label("Фото или видео", systemImage: "photo")
                    }
                    Divider()
                    Button(model.expanded ? "Скрыть поля" : "Кому, копия, тема", systemImage: "list.bullet.rectangle") {
                        withAnimation { model.expanded.toggle() }
                    }
                } label: {
                    Image(systemName: "plus.circle").font(.title2)
                }
                .menuIndicator(.hidden)
                .fixedSize()
                .accessibilityLabel("Вложить или поля письма")

                TextField("Сообщение", text: $model.draft.text, axis: .vertical)
                    .lineLimit(1...8)
                    .textFieldStyle(.plain)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .focused($focused)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 8)
                    .background(RoundedRectangle(cornerRadius: 18).fill(Color.secondary.opacity(0.12)))
                    .onChange(of: model.draft.text) { model.error = nil }
                    .macReturnSends { send() }

                if model.sentFlash && !model.draft.canSend {
                    Label("Отправлено", systemImage: "checkmark.circle.fill")
                        .labelStyle(.iconOnly)
                        .font(.title2)
                        .foregroundStyle(Palette.accent)
                        .transition(.scale.combined(with: .opacity))
                } else {
                    Button("Отправить", systemImage: "arrow.up.circle.fill", action: send)
                        .labelStyle(.iconOnly)
                        .font(.title2)
                        .disabled(!model.draft.canSend)
                        .keyboardShortcut(.return, modifiers: .command)
                }
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .background(.bar)
        .animation(.default, value: model.sentFlash)
        .fileImporter(isPresented: $importing, allowedContentTypes: [.item], allowsMultipleSelection: true) { r in
            if case let .success(urls) = r { model.attach(urls, toSheet: false) }
        }
        .onChange(of: photos) { loadPhotos(into: model, toSheet: false) }
        .onAppear { if autofocus { focused = true } }
    }

    private func send() { onSend() }

    private func loadPhotos(into model: ComposeModel, toSheet: Bool) {
        let items = photos
        photos = []
        PhotoLoader.load(items) { data, name, type in model.attach(data: data, name: name, type: type, toSheet: toSheet) }
    }
}

/// Окно нового письма или пересылки (ветки 0–1).
struct ComposeSheetView: View {
    @Bindable var model: ComposeModel
    @State private var importing = false
    @State private var photos: [PhotosPickerItem] = []
    @FocusState private var toFocused: Bool

    var body: some View {
        if let s = model.sheet {
            NavigationStack {
                Form {
                    Section {
                        LabeledContent("Кому") {
                            TextField("адрес@домен", text: sheetBinding(\.to))
                                .addressInput()
                                .focused($toFocused)
                        }
                        LabeledContent("Копия") {
                            TextField("", text: sheetBinding(\.cc)).addressInput()
                        }
                        LabeledContent("Тема") {
                            TextField(s.original.map { "Fwd: \($0.subject)" } ?? "", text: sheetBinding(\.subject))
                        }
                        FromPicker(selection: sheetBinding(\.from), identities: model.identities)
                    }
                    Section {
                        TextField("Сообщение", text: sheetBinding(\.text), axis: .vertical)
                            .lineLimit(6...30)
                    }
                    if let o = s.original {
                        Section("Пересылаемое письмо") {
                            LabeledContent("От", value: o.from.isEmpty ? o.fromAddr : o.from)
                            LabeledContent("Тема", value: o.subject.isEmpty ? "(без темы)" : o.subject)
                            if !o.attachments.isEmpty {
                                LabeledContent("Вложений", value: "\(o.attachments.count)")
                            }
                        }
                    }
                    if !s.draft.attachments.isEmpty {
                        Section("Вложения") {
                            AttachmentChips(attachments: sheetBinding(\.attachments))
                        }
                    }
                    if let e = s.error {
                        Section { Label(e, systemImage: "exclamationmark.triangle").foregroundStyle(.red) }
                    }
                }
                .formStyle(.grouped)
                .navigationTitle(s.isForward ? "Переслать" : "Новое письмо")
                .toolbarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) {
                        Button("Отмена", systemImage: "xmark") { model.sheet = nil }
                            .disabled(s.busy)
                    }
                    ToolbarItem(placement: .primaryAction) {
                        Menu("Вложить", systemImage: "paperclip") {
                            Button("Файл", systemImage: "doc") { importing = true }
                            PhotosPicker(selection: $photos, matching: .any(of: [.images, .videos])) {
                                Label("Фото или видео", systemImage: "photo")
                            }
                        }
                    }
                    ToolbarItem(placement: .confirmationAction) {
                        if s.busy {
                            ProgressView()
                        } else {
                            Button("Отправить", systemImage: "paperplane.fill") { model.sendSheet() }
                                .keyboardShortcut(.return, modifiers: .command)
                        }
                    }
                }
                .disabled(s.busy)
                .onAppear { toFocused = true }
                .fileImporter(isPresented: $importing, allowedContentTypes: [.item], allowsMultipleSelection: true) { r in
                    if case let .success(urls) = r { model.attach(urls, toSheet: true) }
                }
                .onChange(of: photos) {
                    let items = photos
                    photos = []
                    PhotoLoader.load(items) { data, name, type in model.attach(data: data, name: name, type: type, toSheet: true) }
                }
            }
            .interactiveDismissDisabled(!s.draft.isEmpty || s.busy)
        }
    }

    private func sheetBinding<T>(_ kp: WritableKeyPath<Draft, T>) -> Binding<T> {
        Binding(
            get: { model.sheet?.draft[keyPath: kp] ?? Draft()[keyPath: kp] },
            set: {
                model.sheet?.draft[keyPath: kp] = $0
                model.sheet?.error = nil
            }
        )
    }
}

/// Поля-переопределения ответа: пусто — как выведет ветка отправки (§1).
private struct OverrideFields: View {
    @Binding var draft: Draft
    let identities: [Identity]

    var body: some View {
        VStack(spacing: 4) {
            field("Кому", $draft.to, prompt: "как в беседе")
            field("Копия", $draft.cc, prompt: "")
            HStack {
                Text("Тема").foregroundStyle(.secondary).frame(width: 56, alignment: .leading)
                TextField("Re: …", text: $draft.subject)
            }
            FromPicker(selection: $draft.from, identities: identities)
        }
        .font(.subheadline)
        .textFieldStyle(.roundedBorder)
    }

    private func field(_ label: String, _ text: Binding<String>, prompt: String) -> some View {
        HStack {
            Text(label).foregroundStyle(.secondary).frame(width: 56, alignment: .leading)
            TextField(prompt, text: text).addressInput()
        }
    }
}

/// «От кого»: пусто — адрес выбирается по беседе.
private struct FromPicker: View {
    @Binding var selection: String
    let identities: [Identity]

    var body: some View {
        Picker("От", selection: $selection) {
            Text("Автоматически").tag("")
            ForEach(identities, id: \.email) { i in
                Text(i.name.isEmpty ? i.email : "\(i.name) <\(i.email)>").tag(i.email)
            }
        }
    }
}

/// Приложенные файлы: имя, размер, убрать.
struct AttachmentChips: View {
    @Binding var attachments: [PendingAttachment]

    var body: some View {
        if !attachments.isEmpty {
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: 6) {
                    ForEach(attachments) { a in
                        HStack(spacing: 4) {
                            Image(systemName: "paperclip")
                            Text(a.name).lineLimit(1)
                            Text(ByteCountFormatter.string(fromByteCount: Int64(a.size), countStyle: .file))
                                .foregroundStyle(.secondary)
                            Button("Убрать \(a.name)", systemImage: "xmark.circle.fill") {
                                attachments.removeAll { $0.id == a.id }
                            }
                            .labelStyle(.iconOnly)
                            .foregroundStyle(.secondary)
                        }
                        .font(.caption)
                        .padding(.horizontal, 8)
                        .padding(.vertical, 4)
                        .background(Capsule().fill(Color.secondary.opacity(0.12)))
                    }
                }
            }
        }
    }
}

/// Фото и видео из системного выбора — данными, с именем и типом.
enum PhotoLoader {
    @MainActor
    static func load(_ items: [PhotosPickerItem], into: @escaping @MainActor (Data, String, UTType?) -> Void) {
        for (n, item) in items.enumerated() {
            Task {
                guard let data = try? await item.loadTransferable(type: Data.self) else { return }
                let type = item.supportedContentTypes.first
                let ext = type?.preferredFilenameExtension ?? "jpg"
                into(data, "IMG_\(Int(Date().timeIntervalSince1970))_\(n + 1).\(ext)", type)
            }
        }
    }
}

private extension View {
    func addressInput() -> some View {
        #if os(iOS)
        textContentType(.emailAddress)
            .keyboardType(.emailAddress)
            .textInputAutocapitalization(.never)
            .autocorrectionDisabled()
        #else
        autocorrectionDisabled()
        #endif
    }

    /// Мак: Enter — отправить, Shift+Enter — новая строка (как десктоп, §9).
    @ViewBuilder func macReturnSends(_ send: @escaping () -> Void) -> some View {
        #if os(macOS)
        onKeyPress(.return, phases: .down) { press in
            if press.modifiers.contains(.shift) { return .ignored }
            send()
            return .handled
        }
        #else
        self
        #endif
    }
}
