package ru.letotam.ddmail.settings

import android.content.Context
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import androidx.datastore.preferences.core.booleanPreferencesKey
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.longPreferencesKey
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map

/** Загружать ли письма заранее (контракт §4) — решает пользователь. */
enum class Prefetch { Always, WifiOnly, Never }

private val Context.settingsStore by preferencesDataStore(name = "settings")

/** Настройки приложения (локальные для телефона). */
class AppSettings(private val context: Context) {
    private val prefetchKey = stringPreferencesKey("prefetch")
    private val notifyKey = booleanPreferencesKey("notifications")
    private val notifiedKey = longPreferencesKey("notified_max_uid")
    private val imageSendersKey = androidx.datastore.preferences.core.stringSetPreferencesKey("image_senders")

    /** Отправители, чьи внешние картинки показываем сразу. */
    suspend fun imageSenders(): Set<String> = context.settingsStore.data.first()[imageSendersKey].orEmpty()

    suspend fun allowImagesFrom(addr: String) = context.settingsStore.edit {
        it[imageSendersKey] = it[imageSendersKey].orEmpty() + addr.trim().lowercase()
    }

    /** `null` — ещё не спрашивали: после первого входа спросить. */
    val prefetch: Flow<Prefetch?> = context.settingsStore.data.map { p ->
        p[prefetchKey]?.let { v -> Prefetch.entries.firstOrNull { it.name == v } }
    }

    val notifications: Flow<Boolean> = context.settingsStore.data.map { it[notifyKey] ?: true }

    suspend fun setPrefetch(v: Prefetch) = context.settingsStore.edit { it[prefetchKey] = v.name }

    suspend fun setNotifications(v: Boolean) = context.settingsStore.edit { it[notifyKey] = v }

    private fun notifiedKey(account: String) = longPreferencesKey("notified_max_uid:$account")

    /** Самое новое письмо учётки, о котором уже уведомили (uid). `null` — ещё ни о чём. */
    suspend fun notifiedMax(account: String): Long? = context.settingsStore.data.first()[notifiedKey(account)]

    suspend fun setNotifiedMax(account: String, v: Long) = context.settingsStore.edit { it[notifiedKey(account)] = v }

    /** Учётку удалили — её рубеж уведомлений тоже. */
    suspend fun clearAccountState(account: String) = context.settingsStore.edit {
        it.remove(notifiedKey(account))
        it.remove(notifiedKey)
    }

    /** Можно ли качать тела прямо сейчас — по выбору и по типу сети. */
    suspend fun prefetchAllowedNow(): Boolean = when (prefetch.first()) {
        Prefetch.Always -> true
        Prefetch.WifiOnly -> unmetered()
        Prefetch.Never, null -> false
    }

    fun unmetered(): Boolean {
        val cm = context.getSystemService(ConnectivityManager::class.java) ?: return false
        val caps = cm.getNetworkCapabilities(cm.activeNetwork) ?: return false
        return caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED)
    }
}
