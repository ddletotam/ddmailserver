package ru.letotam.ddmail.ui

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.IntOffset
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.window.Popup
import androidx.compose.ui.window.PopupProperties

/**
 * Бургер (docs/mobile-layout.md): единственная навигация между разделами.
 * Оболочка кладёт сюда «открыть меню», каждый раздел ставит [BurgerButton]
 * в начало своей первой строки — как `view-menu-open` десктопа.
 */
val LocalBurger = staticCompositionLocalOf<(() -> Unit)?> { null }

/** Три полоски в начале первой строки раздела; вне оболочки не рисуется. */
@Composable
fun BurgerButton(modifier: Modifier = Modifier) {
    val open = LocalBurger.current ?: return
    IconButton(onClick = open, modifier = modifier) {
        Icon(Icons.Filled.Menu, "Меню", tint = Color(0xFF6B7480))
    }
}

/** Пункт меню-бургера: иконка, подпись, отметка текущего раздела. */
class BurgerItem(val icon: ImageVector, val title: String, val selected: Boolean, val onClick: () -> Unit)

/**
 * Меню-оверлей бургера — как у десктопа: карточка 220 dp под бургером,
 * выбранный раздел подсвечен. Отдельное окно (Popup): поверх WebView беседы и
 * само закрывается по «Назад» и касанию мимо.
 */
@Composable
fun BurgerMenu(items: List<BurgerItem>, onDismiss: () -> Unit) {
    val d = LocalDensity.current
    val offset = with(d) { IntOffset(8.dp.roundToPx(), 52.dp.roundToPx()) }
    Popup(
        alignment = Alignment.TopStart,
        offset = offset,
        onDismissRequest = onDismiss,
        properties = PopupProperties(focusable = true),
    ) {
        Surface(
            modifier = Modifier.width(220.dp),
            shape = RoundedCornerShape(10.dp),
            color = Color.White,
            border = BorderStroke(1.dp, Color(0xFFE6ECF2)),
            shadowElevation = 8.dp,
        ) {
            Column(Modifier.padding(6.dp), verticalArrangement = Arrangement.spacedBy(2.dp)) {
                items.forEach { item ->
                    Row(
                        Modifier
                            .fillMaxWidth()
                            .height(44.dp)
                            .clip(RoundedCornerShape(8.dp))
                            .background(if (item.selected) Color(0xFFEAF2FE) else Color.Transparent)
                            .clickable {
                                onDismiss()
                                item.onClick()
                            }
                            .padding(horizontal = 12.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        Icon(item.icon, null, tint = if (item.selected) Color(0xFF10B981) else Color(0xFF6B7480), modifier = Modifier.size(20.dp))
                        Spacer(Modifier.width(12.dp))
                        Text(item.title, fontSize = 15.sp, fontWeight = FontWeight.SemiBold, color = Color(0xFF0F1419))
                    }
                }
            }
        }
    }
}
