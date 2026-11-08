package ru.letotam.ddmail.ui

import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color

/** Акцент — тот же зелёный, что у десктопа (`Btn kind 1` в kit.slint). */
private val Light = lightColorScheme(
    primary = Color(0xFF047857),
    onPrimary = Color.White,
    primaryContainer = Color(0xFFD1FAE5),
    onPrimaryContainer = Color(0xFF065F46),
    background = Color(0xFFF6F7F9),
    surface = Color(0xFFF6F7F9),
    error = Color(0xFFC03535),
)

@Composable
fun DdmailTheme(content: @Composable () -> Unit) {
    MaterialTheme(colorScheme = Light, content = content)
}
