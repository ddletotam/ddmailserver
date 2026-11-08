package ru.letotam.ddmail.ui

import androidx.annotation.StringRes
import androidx.compose.runtime.Composable
import androidx.compose.ui.res.stringResource

/** Текст для UI из ViewModel: ресурс + аргументы, без Context во ViewModel. */
data class Msg(@StringRes val id: Int, val args: List<Any> = emptyList()) {
    @Composable
    fun text(): String = stringResource(id, *args.toTypedArray())
}
