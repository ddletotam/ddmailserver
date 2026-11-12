package ru.letotam.ddmail.auth

import android.content.Context
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.builtins.MapSerializer
import kotlinx.serialization.builtins.serializer
import kotlinx.serialization.json.Json
import ru.letotam.ddmail.account.ImapAccount

private val Context.accountsStore by preferencesDataStore(name = "accounts")

/**
 * Внешние IMAP-учётки (mobile-external-accounts §1): список настроек — открыто
 * (адреса серверов, логины), пароли — зашифрованными ключом Android Keystore
 * ([SecretBox], как токен ddmail). Открытым текстом пароль не лежит нигде;
 * без него нет фоновой почты, поэтому он хранится.
 *
 * Учётка ddmail живёт в [SessionStore] (токен, не пароль) — как до внешних
 * учёток; здесь её нет.
 */
class AccountStore(private val context: Context) {
    private val box = SecretBox("ddmail-accounts")
    private val json = Json { ignoreUnknownKeys = true }
    private val listSer = ListSerializer(ImapAccount.serializer())
    private val secretsSer = MapSerializer(String.serializer(), String.serializer())

    val accounts: Flow<List<ImapAccount>> = context.accountsStore.data.map { p ->
        p[LIST]?.let { runCatching { json.decodeFromString(listSer, it) }.getOrNull() }.orEmpty()
    }

    suspend fun list(): List<ImapAccount> = accounts.first()

    /** Пароль учётки; `null` — нет или ключ Keystore пропал (нужен вход). */
    suspend fun password(key: String): String? {
        val p = context.accountsStore.data.first()
        val sealed = p[SECRETS]?.let { runCatching { json.decodeFromString(secretsSer, it) }.getOrNull() }?.get(key) ?: return null
        return box.open(sealed)
    }

    /** Добавить или заменить (по ключу) учётку; [password] `null` — пароль прежний. */
    suspend fun put(account: ImapAccount, password: String?, replacingKey: String? = null) {
        val sealed = password?.let { box.seal(it) }
        context.accountsStore.edit { p ->
            val list = p[LIST]?.let { runCatching { json.decodeFromString(listSer, it) }.getOrNull() }.orEmpty()
            val old = replacingKey ?: account.key
            val idx = list.indexOfFirst { it.key == old }
            val next = if (idx >= 0) list.toMutableList().also { it[idx] = account } else list + account
            p[LIST] = json.encodeToString(listSer, next)
            val secrets = p[SECRETS]?.let { runCatching { json.decodeFromString(secretsSer, it) }.getOrNull() }.orEmpty().toMutableMap()
            if (old != account.key) secrets.remove(old)?.let { if (sealed == null) secrets[account.key] = it }
            if (sealed != null) secrets[account.key] = sealed
            p[SECRETS] = json.encodeToString(secretsSer, secrets)
        }
    }

    /** Удалить учётку вместе с её секретом. */
    suspend fun remove(key: String) {
        context.accountsStore.edit { p ->
            val list = p[LIST]?.let { runCatching { json.decodeFromString(listSer, it) }.getOrNull() }.orEmpty()
            p[LIST] = json.encodeToString(listSer, list.filter { it.key != key })
            val secrets = p[SECRETS]?.let { runCatching { json.decodeFromString(secretsSer, it) }.getOrNull() }.orEmpty() - key
            p[SECRETS] = json.encodeToString(secretsSer, secrets)
        }
    }

    private companion object {
        val LIST = stringPreferencesKey("imap_accounts")
        val SECRETS = stringPreferencesKey("imap_secrets_sealed")
    }
}
