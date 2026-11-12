package ru.letotam.ddmail.auth

import android.content.Context
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.longPreferencesKey
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import kotlinx.coroutines.flow.first

/** Сессия учётки ddmail. Пароль не хранится нигде — только токен (контракт §2). */
data class Session(
    val serverUrl: String,
    val username: String,
    val userId: Long,
    val token: String,
) {
    /** Ключ учётки — как `account_key` десктопа (`логин@хост`). */
    val accountKey: String
        get() = "$username@" + serverUrl.removePrefix("https://").substringBefore(':').substringBefore('/')
}

private val Context.sessionStore by preferencesDataStore(name = "session")

/**
 * Сессия на диске: адрес, логин и id — открыто, токен — зашифрованным
 * AES-GCM ключом из Android Keystore ([SecretBox]). Устаревшую
 * EncryptedSharedPreferences не используем.
 *
 * Не удалось расшифровать (ключ пропал: сброс экрана блокировки, восстановление
 * из бэкапа на другом телефоне) — сессии нет, нужен вход заново.
 */
class SessionStore(private val context: Context) {
    private val box = SecretBox("ddmail-session")

    suspend fun load(): Session? {
        val p = context.sessionStore.data.first()
        val server = p[SERVER] ?: return null
        val username = p[USERNAME] ?: return null
        val sealed = p[TOKEN] ?: return null
        val token = box.open(sealed) ?: return null
        return Session(server, username, p[USER_ID] ?: 0, token)
    }

    suspend fun save(s: Session) {
        val sealed = box.seal(s.token)
        context.sessionStore.edit {
            it[SERVER] = s.serverUrl
            it[USERNAME] = s.username
            it[USER_ID] = s.userId
            it[TOKEN] = sealed
        }
    }

    /** Новый токен после refresh — остальное прежнее. */
    suspend fun updateToken(token: String) {
        val sealed = box.seal(token)
        context.sessionStore.edit { it[TOKEN] = sealed }
    }

    suspend fun clear() {
        context.sessionStore.edit { it.clear() }
    }

    private companion object {
        val SERVER = stringPreferencesKey("server_url")
        val USERNAME = stringPreferencesKey("username")
        val USER_ID = longPreferencesKey("user_id")
        val TOKEN = stringPreferencesKey("token_sealed")
    }
}
