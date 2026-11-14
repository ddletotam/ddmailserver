package ru.letotam.ddmail.mail

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.IntOffset
import androidx.compose.ui.unit.IntRect
import androidx.compose.ui.unit.IntSize
import androidx.compose.ui.unit.LayoutDirection
import androidx.compose.ui.unit.dp
import androidx.compose.ui.window.Popup
import androidx.compose.ui.window.PopupPositionProvider
import androidx.compose.ui.window.PopupProperties
import ru.letotam.ddmail.conversation.MenuAnchor
import ru.letotam.ddmail.conversation.menuPlacement

/**
 * Меню-поповер у элемента ленты (пузыря, чипа вложения) — как
 * `UIContextMenuInteraction` iOS у пузыря и меню по правому клику десктопа,
 * а не диалог посреди экрана. Место — [menuPlacement] по [anchor]; нет
 * якоря (старый документ) — посередине окна. Тап мимо и «Назад» закрывают.
 */
@Composable
fun AnchoredMenu(
    anchor: MenuAnchor?,
    title: String,
    onDismiss: () -> Unit,
    content: @Composable ColumnScope.() -> Unit,
) {
    val density = LocalDensity.current
    val margin = with(density) { 8.dp.roundToPx() }
    val gap = with(density) { 4.dp.roundToPx() }
    val provider = remember(anchor, margin, gap) {
        object : PopupPositionProvider {
            override fun calculatePosition(anchorBounds: IntRect, windowSize: IntSize, layoutDirection: LayoutDirection, popupContentSize: IntSize): IntOffset {
                if (anchor == null) {
                    return IntOffset(
                        ((windowSize.width - popupContentSize.width) / 2).coerceAtLeast(0),
                        ((windowSize.height - popupContentSize.height) / 2).coerceAtLeast(0),
                    )
                }
                val (x, y) = menuPlacement(anchor, popupContentSize.width, popupContentSize.height, windowSize.width, windowSize.height, margin, gap)
                return IntOffset(x, y)
            }
        }
    }
    // Длинное меню («Показать…» раскрыто) на низком экране — прокручивается.
    val config = LocalConfiguration.current
    val maxH = config.screenHeightDp.dp - 16.dp
    val width = minOf(MENU_WIDTH, config.screenWidthDp.dp - 16.dp)
    Popup(popupPositionProvider = provider, onDismissRequest = onDismiss, properties = PopupProperties(focusable = true)) {
        Surface(
            shape = RoundedCornerShape(12.dp),
            color = MaterialTheme.colorScheme.surface,
            shadowElevation = 8.dp,
            modifier = Modifier.width(width).heightIn(max = maxH),
        ) {
            Column(Modifier.verticalScroll(rememberScrollState()).padding(horizontal = 16.dp, vertical = 4.dp)) {
                Text(
                    title,
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 2,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.padding(top = 8.dp, bottom = 4.dp),
                )
                content()
            }
        }
    }
}

private val MENU_WIDTH = 288.dp
