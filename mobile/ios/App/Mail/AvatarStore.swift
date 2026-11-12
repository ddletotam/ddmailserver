import CryptoKit
import DDMailAPI
import Foundation
import SwiftUI

/// Аватарки собеседников (Android §5): `/avatars?email=`, кэш в памяти и на
/// диске, «аватара нет» помнится неделю. SVG (BIMI) не рисуется — инициалы.
/// Спам аватарки не запрашивает: `/avatars` ходит на домен отправителя, то
/// есть по сигналу спамера (решает вызывающий).
actor AvatarStore {
    /// Аватарки отдаёт сервер ddmail; без ddmail-учётки — только инициалы.
    private let api: SessionAPI?
    private var memory: [String: Data] = [:]
    private var missing: Set<String> = []
    private var inflight: [String: Task<Data?, Never>] = [:]
    private let dir = URL.cachesDirectory.appending(path: "avatars")
    private static let negativeTTL: TimeInterval = 7 * 24 * 3600

    init(api: SessionAPI?) {
        self.api = api
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    }

    func image(for email: String) async -> Data? {
        let key = email.lowercased()
        guard !key.isEmpty, let api else { return nil }
        if let d = memory[key] { return d }
        if missing.contains(key) { return nil }
        if let t = inflight[key] { return await t.value }
        let file = dir.appending(path: Self.hash(key))
        let none = dir.appending(path: Self.hash(key) + ".none")
        if let d = try? Data(contentsOf: file) {
            memory[key] = d
            return d
        }
        if let attrs = try? FileManager.default.attributesOfItem(atPath: none.path),
           let date = attrs[.modificationDate] as? Date, Date().timeIntervalSince(date) < Self.negativeTTL {
            missing.insert(key)
            return nil
        }
        let task = Task { [api] () -> Data? in
            guard let b = try? await api.avatar(email: key),
                  b.contentType.lowercased().hasPrefix("image/"), !b.contentType.lowercased().contains("svg")
            else { return nil }
            return b.data
        }
        inflight[key] = task
        let data = await task.value
        inflight[key] = nil
        if let data {
            memory[key] = data
            try? data.write(to: file, options: .atomic)
        } else {
            missing.insert(key)
            FileManager.default.createFile(atPath: none.path, contents: Data())
        }
        return data
    }

    /// Учёток не осталось: чужие аватарки не остаются.
    nonisolated static func clearDisk() {
        try? FileManager.default.removeItem(at: URL.cachesDirectory.appending(path: "avatars"))
    }

    /// Выход из учётки: чужие аватарки не остаются.
    func clear() {
        memory.removeAll()
        missing.removeAll()
        try? FileManager.default.removeItem(at: dir)
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    }

    private static func hash(_ s: String) -> String {
        SHA256.hash(data: Data(s.utf8)).prefix(16).map { String(format: "%02x", $0) }.joined()
    }
}

/// Аватар строки: картинка с сервера, пока её нет — инициалы.
struct ConversationAvatar: View {
    let name: String
    let email: String
    let spam: Bool
    let store: AvatarStore?
    @State private var image: Image?

    var body: some View {
        Group {
            if let image {
                image.resizable().scaledToFill()
                    .frame(width: 40, height: 40)
                    .clipShape(Circle())
                    .accessibilityHidden(true)
            } else {
                Avatar(name: name, seed: email)
            }
        }
        .task(id: email) {
            image = nil
            guard !spam, let store, let data = await store.image(for: email) else { return }
            image = Self.decode(data)
        }
    }

    private static func decode(_ data: Data) -> Image? {
        #if os(iOS)
        UIImage(data: data).map { Image(uiImage: $0) }
        #else
        NSImage(data: data).map { Image(nsImage: $0) }
        #endif
    }
}
