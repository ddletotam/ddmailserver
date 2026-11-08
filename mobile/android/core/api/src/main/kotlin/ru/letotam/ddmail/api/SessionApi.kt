package ru.letotam.ddmail.api

import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.serialization.DeserializationStrategy
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import okhttp3.Call
import okhttp3.Callback
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import java.io.IOException
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

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

    /** Тела писем по ссылкам. Ключ — стабильный `message_id`, uid — запасной. */
    suspend fun messages(refs: List<MessageRef>): List<MessageBody> {
        val body = DdmailApi.json.encodeToString(ListSerializer(MessageRef.serializer()), refs)
        val text = authed { token ->
            Request.Builder().url("$baseUrl/api/desktop/v1/conversations/messages")
                .header("Authorization", "Bearer $token")
                .post(body.toRequestBody(JSON))
                .build()
        }
        return DdmailApi.json.decodeFromString(ListSerializer(MessageBody.serializer()), text)
    }

    /** Поставить или снять флаг (`\\Seen`) на письмах. */
    suspend fun setFlag(refs: List<MessageRef>, flag: String, add: Boolean) {
        val msgs = JsonArray(
            refs.map {
                JsonObject(
                    mapOf(
                        "folder" to JsonPrimitive(it.folder),
                        "uid" to JsonPrimitive(it.uid),
                        "message_id" to JsonPrimitive(it.messageId),
                    ),
                )
            },
        )
        val body = JsonObject(mapOf("messages" to msgs, "flags" to JsonPrimitive(flag), "add" to JsonPrimitive(add)))
        authed { token ->
            Request.Builder().url("$baseUrl/api/desktop/v1/messages/flags")
                .header("Authorization", "Bearer $token")
                .post(body.toString().toRequestBody(JSON))
                .build()
        }
    }

    /** Встроенная часть письма (`cid:`): байты и тип. */
    suspend fun part(uid: Long, cid: String): Binary =
        binary("/messages/$uid/parts/${java.net.URLEncoder.encode(cid, "UTF-8").replace("+", "%20")}")

    /** Вложение по индексу. */
    suspend fun attachment(uid: Long, index: Int): Binary = binary("/messages/$uid/attachments/$index")

    private suspend fun binary(path: String): Binary {
        suspend fun once(token: String): Binary {
            val req = Request.Builder().url("$baseUrl/api/desktop/v1$path").header("Authorization", "Bearer $token").build()
            return http.newCall(req).awaitBinary()
        }
        val token = tokens.current()
        return try {
            once(token)
        } catch (e: ApiException.Unauthorized) {
            once(refreshed(token))
        }
    }

    private suspend fun refreshed(stale: String): String = refreshLock.withLock {
        val now = tokens.current()
        if (now != stale) now else raw.refresh(stale).token.also { tokens.update(it) }
    }

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
            // Пока ждали замок, токен мог уже обменять соседний запрос.
            http.newCall(build(refreshed(token))).awaitBody()
        }
    }

    private companion object {
        val JSON = "application/json; charset=utf-8".toMediaType()
    }
}

/** Двоичный ответ: части письма, вложения. */
class Binary(val bytes: ByteArray, val contentType: String)

internal suspend fun Call.awaitBinary(): Binary = suspendCancellableCoroutine { cont ->
    cont.invokeOnCancellation { cancel() }
    enqueue(object : Callback {
        override fun onFailure(call: Call, e: IOException) {
            if (!cont.isCancelled) cont.resumeWithException(ApiException.Network(e))
        }

        override fun onResponse(call: Call, response: Response) {
            response.use { r ->
                val result = try {
                    when {
                        r.isSuccessful -> Result.success(
                            Binary(r.body?.bytes() ?: ByteArray(0), r.header("Content-Type") ?: "application/octet-stream"),
                        )
                        r.code == 401 || r.code == 403 -> Result.failure(ApiException.Unauthorized(""))
                        else -> Result.failure(ApiException.Server(r.code, ""))
                    }
                } catch (e: IOException) {
                    Result.failure(ApiException.Network(e))
                }
                result.fold({ cont.resume(it) }, { cont.resumeWithException(it) })
            }
        }
    })
}
