import DDMailAPI
import DDMailSync
import SwiftUI

/// Поле поиска в первой строке списка — круглое, на всю ширину справа от
/// бургера (десктоп: сайдбар почты). Return — первая строка выпадашки
/// («Написать по адресу…», если введён адрес).
struct MailSearchField: View {
    @Bindable var search: MailSearch
    let syncing: Bool
    @FocusState private var focused: Bool

    var body: some View {
        let request = search.focusRequest
        let empty = search.query.isEmpty
        HStack(spacing: 6) {
            Image(systemName: "magnifyingglass")
                .foregroundStyle(.secondary)
                .accessibilityHidden(true)
            TextField("Поиск", text: $search.query)
                .textFieldStyle(.plain)
                .focused($focused)
                .autocorrectionDisabled()
                #if os(iOS)
                .textInputAutocapitalization(.never)
                .submitLabel(.search)
                #endif
                .onSubmit { search.submit() }
                // Esc — закрыть выпадашку (как десктоп).
                .onKeyPress(.escape) {
                    search.clear()
                    return .handled
                }
                .accessibilityIdentifier("search.field")
            if !empty {
                Button("Очистить", systemImage: "xmark.circle.fill") { search.clear() }
                    .labelStyle(.iconOnly)
                    .buttonStyle(.plain)
                    .foregroundStyle(.secondary)
            } else if syncing {
                ProgressView().controlSize(.small)
            }
        }
        .padding(.horizontal, 12)
        .frame(height: 36)
        .background(Capsule().fill(Color.secondary.opacity(0.12)))
        .frame(maxWidth: .infinity)
        .onChange(of: request) { focused = true }
    }
}

/// Выпадашка поиска поверх списка: «Написать по адресу…», «Диалоги»,
/// «Контакты», «Сообщения» (десктоп §4 «Поиск»). Высота — по содержимому,
/// длинная прокручивается.
struct SearchDropdown: View {
    let search: MailSearch
    let avatars: AvatarStore?

    var body: some View {
        // Значения — в теле (контракт §0), строки получают готовое.
        let compose = search.composeEmail
        let convs = search.conversations
        let contacts = search.contacts
        let messages = search.messages
        let loading = search.loadingMessages
        let content = SearchResults(compose: compose, convs: convs, contacts: contacts, messages: messages,
                                    loading: loading, search: search, avatars: avatars)
        ViewThatFits(in: .vertical) {
            content
            ScrollView { content }
        }
        .background(RoundedRectangle(cornerRadius: 12).fill(Color.searchCard))
        .overlay(RoundedRectangle(cornerRadius: 12).strokeBorder(Color.secondary.opacity(0.18)))
        .shadow(color: .black.opacity(0.14), radius: 14, y: 4)
        .padding(.horizontal, 10)
        .padding(.top, 2)
        .padding(.bottom, 10)
    }
}

private struct SearchResults: View {
    let compose: String?
    let convs: [ConversationHit]
    let contacts: [MailSearch.ContactHit]
    let messages: [MailSearch.MessageHit]
    let loading: Bool
    let search: MailSearch
    let avatars: AvatarStore?

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            if let e = compose {
                row(id: "search.compose", action: { search.compose() }) {
                    Image(systemName: "square.and.pencil")
                        .frame(width: 28)
                    // Адрес виден целиком: длинный переносится, а не режется.
                    Text("Написать по адресу: \(Text(e).fontWeight(.semibold))")
                        .lineLimit(3)
                        .multilineTextAlignment(.leading)
                    Spacer(minLength: 0)
                }
                .foregroundStyle(Palette.accent)
            }
            if !convs.isEmpty {
                header("Диалоги")
                ForEach(convs, id: \.conversation.ref) { h in
                    let c = h.conversation
                    row(id: "search.conv", action: { search.open(h) }) {
                        ConversationAvatar(name: c.label, email: c.counterparts.first?.addr ?? "", spam: false, store: avatars)
                            .scaleEffect(0.8)
                            .frame(width: 32, height: 32)
                        VStack(alignment: .leading, spacing: 1) {
                            Text(c.label.isEmpty ? (c.counterparts.first?.addr ?? "—") : c.label).lineLimit(1)
                            if !h.matched.isEmpty {
                                Text(h.matched).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                            }
                        }
                        Spacer(minLength: 0)
                    }
                }
            }
            if !contacts.isEmpty {
                header("Контакты")
                ForEach(contacts) { k in
                    row(id: "search.contact", action: { search.write(k) }) {
                        ConversationAvatar(name: k.name.isEmpty ? k.email : k.name, email: k.email, spam: false, store: avatars)
                            .scaleEffect(0.8)
                            .frame(width: 32, height: 32)
                        VStack(alignment: .leading, spacing: 1) {
                            Text(k.name.isEmpty ? k.email : k.name).lineLimit(1)
                            if !k.name.isEmpty {
                                Text(k.email).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                            }
                        }
                        Spacer(minLength: 0)
                    }
                }
            }
            if !messages.isEmpty {
                header("Сообщения")
                ForEach(messages) { m in
                    row(id: "search.message", action: { search.open(m) }) {
                        Image(systemName: "envelope")
                            .foregroundStyle(.secondary)
                            .frame(width: 32)
                        VStack(alignment: .leading, spacing: 1) {
                            Text(m.hit.subject.isEmpty ? "(без темы)" : m.hit.subject).lineLimit(1)
                            Text(m.hit.from).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                        }
                        Spacer(minLength: 4)
                        Text(RowDate.format(m.hit.date)).font(.caption).foregroundStyle(.secondary)
                    }
                }
            }
            if compose == nil && convs.isEmpty && contacts.isEmpty && messages.isEmpty {
                Text(loading ? "Ищу…" : "Ничего не найдено")
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity)
                    .padding(.vertical, 12)
            } else if loading && messages.isEmpty {
                header("Сообщения")
                Text("Ищу…")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, 12)
                    .padding(.bottom, 8)
            }
        }
        .padding(4)
    }

    private func header(_ title: String) -> some View {
        Text(title)
            .font(.caption.weight(.semibold))
            .foregroundStyle(.secondary)
            .padding(.horizontal, 12)
            .padding(.top, 10)
            .padding(.bottom, 4)
    }

    private func row(id: String, action: @escaping () -> Void, @ViewBuilder _ label: () -> some View) -> some View {
        Button(action: action) {
            HStack(spacing: 10) { label() }
                .padding(.horizontal, 12)
                .frame(minHeight: 44)
                .contentShape(Rectangle())
        }
        .buttonStyle(SearchRowStyle())
        .accessibilityIdentifier(id)
    }
}

/// Подсветка нажатой строки выпадашки.
private struct SearchRowStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .background(RoundedRectangle(cornerRadius: 8).fill(configuration.isPressed ? Palette.accent.opacity(0.14) : .clear))
    }
}

extension Color {
    /// Фон выпадашки — как у всплывающих панелей платформы.
    static var searchCard: Color {
        #if os(iOS)
        Color(uiColor: .secondarySystemGroupedBackground)
        #else
        Color(nsColor: .controlBackgroundColor)
        #endif
    }
}
