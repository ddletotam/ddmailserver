import DDMailAPI
import DDMailSync
import PhotosUI
import SwiftUI
import UniformTypeIdentifiers

/// Строка ответа под лентой (§5, §9): текст с форматированием, картинки и
/// вложения, отправка кнопкой. На iPhone Enter — новая строка; на Маке Enter
/// — отправить, Shift+Enter — новая строка (как десктоп); с клавиатурой у
/// iPad — Cmd+Enter.
///
/// Форматирование (десктоп §3а) — без своей строки кнопок: на iPhone и iPad
/// системное меню выделения («Формат» поля `TextEditor` над
/// `AttributedString`), на Маке и с клавиатурой — ⌘B/⌘I/⌘U, ссылка — ⌘K или
/// «+» → «Ссылка из буфера». Картинки — «+» → «Картинка из буфера» или из
/// «Фото»: своим блоком под текстом, уходят inline (`cid:`).
struct ComposerBar: View {
    @Bindable var model: ComposeModel
    /// Фокус в строку сразу (новая беседа: адрес выбран — пишем).
    var autofocus = false
    /// Ответ в беседе или первое письмо новой беседы; контекст шрифтов —
    /// чтобы понять, где жирный и курсив.
    let onSend: (Font.Context) -> Void
    @State private var importing = false
    @State private var photos: [PhotosPickerItem] = []
    @FocusState private var focused: Bool
    @Environment(\.fontResolutionContext) private var fontContext

    var body: some View {
        // Наблюдаемое — в теле (контракт §0): замыкания `overlay` ленивы.
        let flash = model.sentFlash
        let text = model.draft.text
        let canSend = model.draft.canSend
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
            InlineImages(images: $model.draft.images)
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
                    Button("Картинка из буфера", systemImage: "photo.on.rectangle") { model.pasteImage() }
                    Button("Ссылка из буфера", systemImage: "link") { model.linkFromPasteboard() }
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

                editor(text: text)

                Button("Отправить", systemImage: "arrow.up.circle.fill") { onSend(fontContext) }
                    .labelStyle(.iconOnly)
                    .font(.title2)
                    .disabled(!canSend)
                    .keyboardShortcut(.return, modifiers: .command)
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .background(.bar)
        // «✓ Отправлено» над композером ~2 с, без тостов (десктоп §2).
        .overlay(alignment: .top) {
            if flash {
                Label("Отправлено", systemImage: "checkmark.circle.fill")
                    .font(.subheadline.weight(.semibold))
                    .foregroundStyle(.white)
                    .padding(.horizontal, 14)
                    .padding(.vertical, 6)
                    .frame(height: 32)
                    .background(Capsule().fill(Palette.accent))
                    // Над строкой ввода, не поверх неё.
                    .offset(y: -40)
                    .transition(.move(edge: .bottom).combined(with: .opacity))
                    .accessibilityIdentifier("compose.sent")
            }
        }
        .animation(.default, value: flash)
        .focusedValue(\.composerFormat, format)
        .fileImporter(isPresented: $importing, allowedContentTypes: [.item], allowsMultipleSelection: true) { r in
            if case let .success(urls) = r { model.attach(urls, toSheet: false) }
        }
        .onChange(of: photos) { loadPhotos() }
        .onAppear { if autofocus { focused = true } }
    }

    /// Поле ввода растёт со строками до восьми, дальше прокручивается.
    /// Высоту задаёт невидимый текст того же содержимого: у `TextEditor`
    /// своего «по содержимому» нет.
    private func editor(text: String) -> some View {
        Text(text.isEmpty ? " " : text + " ")
            .lineLimit(1...8)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, 5)
            .padding(.vertical, 8)
            .hidden()
            .overlay {
                TextEditor(text: $model.rich, selection: $model.selection)
                    .scrollContentBackground(.hidden)
                    .focused($focused)
                    .onChange(of: model.rich) { model.error = nil }
                    .macReturnSends { onSend(fontContext) }
                    .accessibilityLabel("Сообщение")
                    .accessibilityIdentifier("composer.text")
            }
            .overlay(alignment: .topLeading) {
                if text.isEmpty {
                    Text("Сообщение")
                        .foregroundStyle(.tertiary)
                        .padding(.horizontal, 5)
                        .padding(.vertical, 8)
                        .allowsHitTesting(false)
                        .accessibilityHidden(true)
                }
            }
            .padding(.horizontal, 7)
            .background(RoundedRectangle(cornerRadius: 18).fill(Color.secondary.opacity(0.12)))
    }

    /// Команды формата для меню приложения (⌘B/⌘I/⌘U/⌘K).
    private var format: ComposerFormat {
        let ctx = fontContext
        let m = model
        return ComposerFormat(
            bold: { m.toggle(.bold, ctx) },
            italic: { m.toggle(.italic, ctx) },
            underline: { m.toggle(.underline, ctx) },
            link: { m.linkFromPasteboard() }
        )
    }

    /// Снимки — в тело письма картинками (HEIC — в JPEG: его понимают не все
    /// почтовики), видео — вложениями.
    private func loadPhotos() {
        let items = photos
        photos = []
        let m = model
        PhotoLoader.load(items) { data, name, type in
            if let type, type.conforms(to: .image), let n = InlineImageData.normalize(data, type: type) {
                let base = (name as NSString).deletingPathExtension
                m.addImage(data: n.data, name: "\(base).\(n.ext)", mime: n.mime)
            } else {
                m.attach(data: data, name: name, type: type, toSheet: false)
            }
        }
    }
}

/// Картинки тела письма: что понимает любой почтовик — как есть, прочее
/// (HEIC и т. п.) — в JPEG.
enum InlineImageData {
    static func normalize(_ data: Data, type: UTType) -> (data: Data, mime: String, ext: String)? {
        if type.conforms(to: .png) { return (data, "image/png", "png") }
        if type.conforms(to: .jpeg) { return (data, "image/jpeg", "jpg") }
        if type.conforms(to: .gif) { return (data, "image/gif", "gif") }
        #if os(iOS)
        guard let jpg = UIImage(data: data)?.jpegData(compressionQuality: 0.85) else { return nil }
        #else
        guard let rep = NSImage(data: data)?.tiffRepresentation.flatMap(NSBitmapImageRep.init(data:)),
              let jpg = rep.representation(using: .jpeg, properties: [.compressionFactor: 0.85])
        else { return nil }
        #endif
        return (jpg, "image/jpeg", "jpg")
    }
}

/// Картинки тела: миниатюры блоками под текстом, «✕» — убрать.
struct InlineImages: View {
    @Binding var images: [PendingAttachment]

    var body: some View {
        if !images.isEmpty {
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: 8) {
                    ForEach(images) { a in
                        thumbnail(a.data)
                            .frame(width: 64, height: 64)
                            .clipShape(RoundedRectangle(cornerRadius: 8))
                            .overlay(alignment: .topTrailing) {
                                Button("Убрать картинку", systemImage: "xmark.circle.fill") {
                                    images.removeAll { $0.id == a.id }
                                }
                                .labelStyle(.iconOnly)
                                .foregroundStyle(.white, .black.opacity(0.55))
                                .padding(2)
                            }
                            .accessibilityElement(children: .contain)
                            .accessibilityLabel("Картинка \(a.name)")
                            .accessibilityIdentifier("composer.image")
                    }
                }
            }
        }
    }

    @ViewBuilder private func thumbnail(_ data: Data) -> some View {
        #if os(iOS)
        if let i = UIImage(data: data) { Image(uiImage: i).resizable().scaledToFill() } else { Color.secondary.opacity(0.2) }
        #else
        if let i = NSImage(data: data) { Image(nsImage: i).resizable().scaledToFill() } else { Color.secondary.opacity(0.2) }
        #endif
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
                TextField("Re: …", text: $draft.subject).accessibilityIdentifier("composer.subject")
            }
            // Подпись — как у «Кому / Копия / Тема»: без неё «Автоматически ⇕»
            // висело без объяснения, что это.
            HStack {
                Text("От").foregroundStyle(.secondary).frame(width: 56, alignment: .leading)
                FromPicker(selection: $draft.from, identities: identities)
                    .labelsHidden()
                Spacer(minLength: 0)
            }
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
