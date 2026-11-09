package ru.letotam.ddmail.ui

import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.ime
import androidx.compose.runtime.Composable
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.unit.dp

/**
 * Клавиатура заняла больше половины окна — на разложенном Mate XT EMUI-шная
 * занимает ~85% высоты. Тогда формы прячут всё, кроме полей ввода (§6).
 */
@Composable
fun imeCrowds(): Boolean {
    val density = LocalDensity.current
    val imePx = WindowInsets.ime.getBottom(density)
    val heightPx = with(density) { LocalConfiguration.current.screenHeightDp.dp.toPx() }
    return imePx > heightPx / 2
}
