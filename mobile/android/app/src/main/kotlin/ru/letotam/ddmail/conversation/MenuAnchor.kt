package ru.letotam.ddmail.conversation

import kotlin.math.roundToInt

/**
 * Где показать меню элемента ленты (пузыря, чипа вложения) — в пикселях окна:
 * прямоугольник элемента, точка нажатия и видимая часть ленты (сам WebView).
 * Пузырь бывает выше экрана — тогда видна только его часть внутри `view*`.
 */
data class MenuAnchor(
    val left: Int,
    val top: Int,
    val right: Int,
    val bottom: Int,
    val touchX: Int,
    val touchY: Int,
    val viewLeft: Int,
    val viewTop: Int,
    val viewRight: Int,
    val viewBottom: Int,
) {
    companion object {
        /**
         * Из координат документа (`askMenu` в `conversation.js`): CSS px от
         * левого верхнего угла видимой области и её ширина [viewportCss] в CSS
         * px. Масштаб — ширина WebView в пикселях к ней: так учтены и
         * плотность экрана, и щипок; прокрутку документ уже вычел.
         * [viewX]/[viewY] — левый верхний угол WebView в окне.
         */
        fun fromDocument(
            viewX: Int, viewY: Int, viewWidth: Int, viewHeight: Int,
            x: Double, y: Double, w: Double, h: Double, px: Double, py: Double, viewportCss: Double,
        ): MenuAnchor? {
            if (!(viewportCss > 0) || viewWidth <= 0 || viewHeight <= 0) return null
            if (listOf(x, y, w, h, px, py).any { it.isNaN() || it.isInfinite() }) return null
            val k = viewWidth / viewportCss
            fun sx(v: Double) = viewX + (v * k).roundToInt()
            fun sy(v: Double) = viewY + (v * k).roundToInt()
            return MenuAnchor(
                left = sx(x), top = sy(y), right = sx(x + w), bottom = sy(y + h),
                touchX = sx(px), touchY = sy(py),
                viewLeft = viewX, viewTop = viewY, viewRight = viewX + viewWidth, viewBottom = viewY + viewHeight,
            )
        }
    }
}

/**
 * Левый верхний угол меню размером [menuW]×[menuH] у элемента [a] в окне
 * [windowW]×[windowH] (как у iOS — у пузыря, а не посреди экрана):
 * - по горизонтали — по краю пузыря с его стороны (входящие слева,
 *   исходящие справа);
 * - по вертикали — под видимым низом пузыря; не влезает — над видимым
 *   верхом; пузырь закрывает ленту целиком — у точки нажатия;
 * - всегда целиком в окне с полем [margin]; меню выше окна — от верхнего
 *   поля (оно тогда прокручивается).
 */
fun menuPlacement(a: MenuAnchor, menuW: Int, menuH: Int, windowW: Int, windowH: Int, margin: Int, gap: Int): Pair<Int, Int> {
    val bubbleCenter = (a.left + a.right) / 2
    val viewCenter = (a.viewLeft + a.viewRight) / 2
    val x0 = if (bubbleCenter > viewCenter) a.right - menuW else a.left
    val x = x0.coerceIn(margin, maxOf(margin, windowW - menuW - margin))

    val maxY = windowH - menuH - margin
    val bottomVisible = a.bottom in a.viewTop..a.viewBottom
    val topVisible = a.top in a.viewTop..a.viewBottom
    val below = a.bottom + gap
    val above = a.top - gap - menuH
    val y0 = when {
        bottomVisible && below <= maxY -> below
        topVisible && above >= margin -> above
        else -> a.touchY
    }
    val y = y0.coerceIn(margin, maxOf(margin, maxY))
    return x to y
}
