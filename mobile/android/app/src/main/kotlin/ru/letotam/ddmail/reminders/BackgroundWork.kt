package ru.letotam.ddmail.reminders

import android.annotation.SuppressLint
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.PowerManager
import android.provider.Settings
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.BatteryChargingFull
import androidx.compose.material.icons.filled.CheckCircle
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.RocketLaunch
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.lifecycle.compose.LifecycleResumeEffect

/**
 * Работа в фоне (контракт §13). Экономия батареи откладывает будильники и
 * фоновую синхронизацию; исключение из неё — системный диалог. Поверх
 * Android у EMUI свой «Запуск приложений»: при автоматическом управлении он
 * выгружает процесс, и программно его не переключить — только открыть экран
 * и подсказать, что выставить.
 */
object BackgroundWork {
    fun unrestricted(context: Context): Boolean =
        context.getSystemService(PowerManager::class.java)?.isIgnoringBatteryOptimizations(context.packageName) == true

    @SuppressLint("BatteryLife") // календарь с напоминаниями — законный случай; в AppGallery разрешено
    fun requestUnrestricted(context: Context) {
        val direct = Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:${context.packageName}"))
        if (!start(context, direct)) start(context, Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS))
    }

    fun isHuawei(): Boolean = android.os.Build.MANUFACTURER.equals("HUAWEI", ignoreCase = true) ||
        android.os.Build.MANUFACTURER.equals("HONOR", ignoreCase = true)

    /** Экран «Запуск приложений» EMUI; нет такого — карточка приложения в настройках. */
    fun openLaunchManager(context: Context) {
        val candidates = listOf(
            ComponentName("com.huawei.systemmanager", "com.huawei.systemmanager.startupmgr.ui.StartupNormalAppListActivity"),
            ComponentName("com.huawei.systemmanager", "com.huawei.systemmanager.appcontrol.activity.StartupAppControlActivity"),
            ComponentName("com.huawei.systemmanager", "com.huawei.systemmanager.optimize.process.ProtectActivity"),
        )
        for (c in candidates) if (start(context, Intent().setComponent(c))) return
        start(context, Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:${context.packageName}")))
    }

    private fun start(context: Context, i: Intent): Boolean = try {
        context.startActivity(i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
        true
    } catch (_: Exception) {
        false
    }
}

/** Пояснение и две кнопки: «не ограничивать» и (на Huawei) «запуск приложений». */
@Composable
fun BackgroundWorkSteps() {
    val context = LocalContext.current
    var free by remember { mutableStateOf(BackgroundWork.unrestricted(context)) }
    // Вернулись из системного диалога — перечитать состояние.
    LifecycleResumeEffect(Unit) {
        free = BackgroundWork.unrestricted(context)
        onPauseOrDispose { }
    }
    Column(verticalArrangement = Arrangement.spacedBy(10.dp)) {
        Text(
            "Чтобы напоминания о встречах приходили вовремя, а почта проверялась в фоне, системе нельзя «усыплять» приложение.",
            fontSize = 14.sp,
        )
        Step(Icons.Filled.BatteryChargingFull, "Не ограничивать в фоне", done = free) { BackgroundWork.requestUnrestricted(context) }
        if (BackgroundWork.isHuawei()) {
            Step(Icons.Filled.RocketLaunch, "Запуск приложений", done = false) { BackgroundWork.openLaunchManager(context) }
            Text(
                "Там у ddmail выключите «Автоматически» и оставьте включёнными все три: автозапуск, косвенный запуск, работа в фоне.",
                fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

@Composable
private fun Step(icon: ImageVector, label: String, done: Boolean, click: () -> Unit) {
    Row(verticalAlignment = Alignment.CenterVertically) {
        FilledTonalButton(onClick = click, enabled = !done, modifier = Modifier.weight(1f)) {
            Icon(icon, null)
            Spacer(Modifier.width(8.dp))
            Text(label)
        }
        if (done) Icon(Icons.Filled.CheckCircle, "Готово", Modifier.padding(start = 8.dp), tint = MaterialTheme.colorScheme.primary)
    }
}

/** Один раз при первом открытии календаря — если ограничения ещё действуют. */
@Composable
fun BackgroundWorkDialog(onClose: () -> Unit) {
    AlertDialog(
        onDismissRequest = onClose,
        icon = { Icon(Icons.Filled.BatteryChargingFull, null) },
        title = { Text("Напоминания в фоне") },
        text = { Column(Modifier.fillMaxWidth()) { BackgroundWorkSteps() } },
        confirmButton = { IconButton(onClick = onClose) { Icon(Icons.Filled.Close, "Закрыть") } },
    )
}
