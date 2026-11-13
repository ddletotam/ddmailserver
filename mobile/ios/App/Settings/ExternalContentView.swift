import SwiftUI

/// «Настройки → Внешний контент» (десктоп §5г, вкладка 1): от кого картинки
/// показываются всегда (кнопка «Всегда от отправителя» под письмом) — список
/// с отзывом. Разрешения живут на устройстве (`ImageSenders`).
struct ExternalContentView: View {
    @State private var senders: [String] = ImageSenders.all.sorted()

    var body: some View {
        Form {
            Section {
                if senders.isEmpty {
                    Text("Пока никого. Под письмом с картинками — «Всегда показывать картинки от отправителя».")
                        .foregroundStyle(.secondary)
                } else {
                    ForEach(senders, id: \.self) { addr in
                        HStack {
                            Text(addr).lineLimit(1).truncationMode(.middle)
                            Spacer()
                            Button("Отозвать", systemImage: "xmark.circle") { revoke([addr]) }
                                .labelStyle(.iconOnly)
                                .buttonStyle(.borderless)
                                .foregroundStyle(.secondary)
                                .accessibilityLabel("Отозвать: \(addr)")
                        }
                        .accessibilityIdentifier("external.sender")
                    }
                    .onDelete { revoke($0.map { senders[$0] }) }
                }
            } header: {
                Text("Всегда показывать картинки от")
            } footer: {
                Text("Внешняя картинка сообщает отправителю, что письмо открыто, и раскрывает ваш IP-адрес. Отозванным — снова по нажатию «Показать картинки». В «Спаме» разрешения не действуют.")
            }
            if senders.count > 1 {
                Section {
                    Button("Отозвать все", systemImage: "trash", role: .destructive) { revoke(senders) }
                }
            }
        }
        .formStyle(.grouped)
        .navigationTitle("Внешний контент")
        .toolbarTitleDisplayMode(.inline)
        .onAppear { senders = ImageSenders.all.sorted() }
    }

    private func revoke(_ addrs: [String]) {
        ImageSenders.revoke(addrs)
        senders = ImageSenders.all.sorted()
    }
}
