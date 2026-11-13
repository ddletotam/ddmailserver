import SwiftUI

/// Навигация между разделами — одна на всё приложение: бургер
/// (`docs/mobile-layout.md`, десктоп `view-menu-open`). Раздел получает её из
/// окружения и ставит бургер в свою первую строку.
struct SectionNav {
    var current: AppSection
    var go: (AppSection) -> Void
    var settings: () -> Void
}

extension EnvironmentValues {
    @Entry var sectionNav: SectionNav?
}

/// Бургер: меню поверх раздела — «Почта, Календарь, Контакты, Задачи,
/// Настройки», текущий раздел отмечен. Системное меню, а не своё окно:
/// так оно ведёт себя как меню платформы (закрывается касанием мимо, Esc,
/// озвучивается) и на iPhone, и на iPad, и на Маке.
struct BurgerMenu: View {
    @Environment(\.sectionNav) private var nav

    var body: some View {
        let current = nav?.current
        Menu {
            Picker("Раздел", selection: Binding(get: { current ?? .mail }, set: { nav?.go($0) })) {
                ForEach(AppSection.allCases) { s in
                    Label(s.title, systemImage: s.symbol).tag(s)
                }
            }
            .pickerStyle(.inline)
            .labelsHidden()
            Button("Настройки", systemImage: "gearshape") { nav?.settings() }
        } label: {
            Image(systemName: "line.3.horizontal")
                .font(.system(size: 18, weight: .medium))
                .foregroundStyle(.secondary)
                .frame(width: 36, height: 36)
                .contentShape(Rectangle())
        }
        .menuIndicator(.hidden)
        // Серые полоски, как у десктопа, а не цвет акцента.
        .menuStyle(.button)
        .buttonStyle(.plain)
        .fixedSize()
        .accessibilityLabel("Разделы")
        .accessibilityIdentifier("nav.burger")
    }
}

/// Первая строка раздела: бургер слева, дальше — своё содержимое раздела
/// (поле поиска у почты, навигация по датам у календаря…). Над ней ничего.
struct SectionBar<Content: View>: View {
    @ViewBuilder var content: Content

    var body: some View {
        HStack(spacing: 8) {
            BurgerMenu()
            content
        }
        .padding(.leading, 8)
        .padding(.trailing, 10)
        .frame(minHeight: 52)
    }
}

extension View {
    /// Первую строку раздела рисует сам раздел (`SectionBar`): системной
    /// панели навигации над ней нет. Экраны, открытые поверх (беседа, день
    /// календаря, контакт), свою панель с «‹» сохраняют.
    @ViewBuilder func sectionRoot() -> some View {
        #if os(iOS)
        toolbar(.hidden, for: .navigationBar)
        #else
        self
        #endif
    }
}
