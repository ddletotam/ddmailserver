package ru.letotam.ddmail.calendar

import android.content.Context
import androidx.datastore.preferences.core.booleanPreferencesKey
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.core.stringSetPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map

private val Context.calendarStore by preferencesDataStore(name = "calendar")

/**
 * Локальные настройки календаря (контракт §12). Видимость календаря — только
 * на этом телефоне, как у десктопа: серверный `enabled` не трогается, синк
 * скрытого идёт дальше, но он не рисуется и не звонит.
 */
class CalendarPrefs(private val context: Context) {
    private val hiddenKey = stringSetPreferencesKey("hidden_calendars")
    private val defaultKey = androidx.datastore.preferences.core.longPreferencesKey("default_calendar")
    private val defaultBookKey = androidx.datastore.preferences.core.longPreferencesKey("default_address_book")
    private val remindersKey = booleanPreferencesKey("reminders")
    private val batteryAskedKey = booleanPreferencesKey("battery_asked")
    private val viewKey = stringPreferencesKey("view")
    private val doneKey = booleanPreferencesKey("tasks_completed")

    val hidden: Flow<Set<Long>> = context.calendarStore.data.map { p -> p[hiddenKey].orEmpty().mapNotNull { it.toLongOrNull() }.toSet() }

    suspend fun hiddenNow(): Set<Long> = hidden.first()

    suspend fun setHidden(id: Long, hide: Boolean) = context.calendarStore.edit {
        val cur = it[hiddenKey].orEmpty()
        it[hiddenKey] = if (hide) cur + id.toString() else cur - id.toString()
    }

    val remindersOn: Flow<Boolean> = context.calendarStore.data.map { it[remindersKey] ?: true }

    suspend fun setRemindersOn(v: Boolean) = context.calendarStore.edit { it[remindersKey] = v }

    /** Спросили ли уже про работу в фоне (батарея, автозапуск EMUI). */
    suspend fun batteryAsked(): Boolean = context.calendarStore.data.first()[batteryAskedKey] ?: false

    suspend fun setBatteryAsked() = context.calendarStore.edit { it[batteryAskedKey] = true }

    /** Последний вид календаря — открывается тем же. */
    suspend fun view(): String? = context.calendarStore.data.first()[viewKey]

    suspend fun setView(v: String) = context.calendarStore.edit { it[viewKey] = v }

    val showCompleted: Flow<Boolean> = context.calendarStore.data.map { it[doneKey] ?: false }

    suspend fun setShowCompleted(v: Boolean) = context.calendarStore.edit { it[doneKey] = v }

    /** Выход из учётки: id календарей чужой сессии бессмысленны. */
    /**
     * Календарь, куда пишутся новые события. Не назначен — форма не выбирает
     * сама: молча взятый «первый записываемый» мог оказаться рабочим, и
     * личное событие ушло бы на чужой сервер.
     */
    val defaultCalendar: Flow<Long?> = context.calendarStore.data.map { it[defaultKey] }

    suspend fun setDefaultCalendar(id: Long?) = context.calendarStore.edit { if (id == null) it.remove(defaultKey) else it[defaultKey] = id }

    /** Адресная книга для новых контактов — тот же принцип, что у календаря (§15). */
    val defaultAddressBook: Flow<Long?> = context.calendarStore.data.map { it[defaultBookKey] }

    suspend fun setDefaultAddressBook(id: Long?) = context.calendarStore.edit { if (id == null) it.remove(defaultBookKey) else it[defaultBookKey] = id }

    suspend fun clear() = context.calendarStore.edit {
        it.remove(defaultBookKey)
        it.remove(hiddenKey)
        it.remove(defaultKey)
        it.remove(viewKey)
    }
}
