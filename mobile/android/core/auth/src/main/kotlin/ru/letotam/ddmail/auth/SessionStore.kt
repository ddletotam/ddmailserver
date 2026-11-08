package ru.letotam.ddmail.auth

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.longPreferencesKey
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import kotlinx.coroutines.flow.first
import java.security.KeyStore
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

/** Сессия учётки ddmail. Пароль не хранится нигде — только токен (контракт §2). */
data class Session(
    val serverUrl: String,
    val username: String,
    val userId: Long,
    val token: String,
)

private val Context.sessionStore by preferencesDataStore(name = "session")

/**
 * Сессия на диске: адрес, логин и id — открыто, токен — зашифрованным
 * AES-GCM ключом из Android Keystore (ключ из аппаратного хранилища не
 * выходит). Устаревшую EncryptedSharedPreferences не используем.
 *
 * Не удалось расшифровать (ключ пропал: сброс экрана блокировки, восстановление
 * из бэкапа на другом телефоне) — сессии нет, нужен вход заново.
 */
class SessionStore(private val context: Context) {

    suspend fun load(): Session? {
        val p = context.sessionStore.data.first()
        val server = p[SERVER] ?: return null
        val username = p[USERNAME] ?: return null
        val sealed = p[TOKEN] ?: return null
        val token = runCatching { open(sealed) }.getOrNull() ?: return null
        return Session(server, username, p[USER_ID] ?: 0, token)
    }

    suspend fun save(s: Session) {
        val sealed = seal(s.token)
        context.sessionStore.edit {
            it[SERVER] = s.serverUrl
            it[USERNAME] = s.username
            it[USER_ID] = s.userId
            it[TOKEN] = sealed
        }
    }

    /** Новый токен после refresh — остальное прежнее. */
    suspend fun updateToken(token: String) {
        val sealed = seal(token)
        context.sessionStore.edit { it[TOKEN] = sealed }
    }

    suspend fun clear() {
        context.sessionStore.edit { it.clear() }
    }

    private fun key(): SecretKey {
        val ks = KeyStore.getInstance(KEYSTORE).apply { load(null) }
        (ks.getEntry(ALIAS, null) as? KeyStore.SecretKeyEntry)?.let { return it.secretKey }
        val gen = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, KEYSTORE)
        gen.init(
            KeyGenParameterSpec.Builder(ALIAS, KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT)
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .setKeySize(256)
                .build(),
        )
        return gen.generateKey()
    }

    /** iv (12 байт) + шифротекст с тегом, в base64. */
    private fun seal(plain: String): String {
        val c = Cipher.getInstance(TRANSFORM).apply { init(Cipher.ENCRYPT_MODE, key()) }
        val out = c.iv + c.doFinal(plain.toByteArray(Charsets.UTF_8))
        return Base64.encodeToString(out, Base64.NO_WRAP)
    }

    private fun open(sealed: String): String {
        val raw = Base64.decode(sealed, Base64.NO_WRAP)
        val c = Cipher.getInstance(TRANSFORM)
        c.init(Cipher.DECRYPT_MODE, key(), GCMParameterSpec(128, raw, 0, 12))
        return String(c.doFinal(raw, 12, raw.size - 12), Charsets.UTF_8)
    }

    private companion object {
        const val KEYSTORE = "AndroidKeyStore"
        const val ALIAS = "ddmail-session"
        const val TRANSFORM = "AES/GCM/NoPadding"
        val SERVER = stringPreferencesKey("server_url")
        val USERNAME = stringPreferencesKey("username")
        val USER_ID = longPreferencesKey("user_id")
        val TOKEN = stringPreferencesKey("token_sealed")
    }
}
