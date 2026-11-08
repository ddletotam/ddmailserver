package ru.letotam.ddmail.api

import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.serialization.DeserializationStrategy
import okhttp3.OkHttpClient
import okhttp3.Request

/** Откуда API берёт токен и куда кладёт обновлённый. */
interface TokenSource {
    suspend fun current(): String
    suspend fun update(token: String)
}

/**
 * Запросы от имени сессии. На 401 — один раз обменять токен через
 * `/auth/refresh` и повторить; refresh тоже 401/403 — сессия мертва,
 * [ApiException.Unauthorized] уходит наверх (контракт §2: «нужен вход»).
 * Обмен сериализован: десять запросов, разом упёршихся в 401, делают один
 * refresh, а не десять.
 */
class SessionApi(
    private val baseUrl: String,
    private val http: OkHttpClient,
    private val tokens: TokenSource,
) {
    private val raw = DdmailApi(baseUrl, http)
    private val refreshLock = Mutex()

    suspend fun conversationsDelta(sinceMs: Long, limit: Int = 5000): ConversationsDelta =
        get("/conversations?limit=$limit&since=${sinceMs.coerceAtLeast(0)}", ConversationsDelta.serializer())

    suspend fun changes(since: Long): ChangesResponse =
        get("/changes?since=${since.coerceAtLeast(0)}", ChangesResponse.serializer())

    suspend fun dialogFolders(): DialogFoldersResponse =
        get("/dialog-folders", DialogFoldersResponse.serializer())

    private suspend fun <T> get(path: String, de: DeserializationStrategy<T>): T {
        val body = authed { token ->
            Request.Builder().url("$baseUrl/api/desktop/v1$path").header("Authorization", "Bearer $token").build()
        }
        return DdmailApi.json.decodeFromString(de, body)
    }

    private suspend fun authed(build: (String) -> Request): String {
        val token = tokens.current()
        return try {
            http.newCall(build(token)).awaitBody()
        } catch (e: ApiException.Unauthorized) {
            val fresh = refreshLock.withLock {
                val now = tokens.current()
                // Пока ждали замок, токен уже обменял соседний запрос.
                if (now != token) now else raw.refresh(token).token.also { tokens.update(it) }
            }
            http.newCall(build(fresh)).awaitBody()
        }
    }
}
