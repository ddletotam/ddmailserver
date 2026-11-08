package ru.letotam.ddmail.api

import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.boolean
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
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

/** Ответ входа и обмена токена (`/auth/login`, `/auth/refresh`). */
@Serializable
data class LoginResponse(
    val token: String,
    @SerialName("user_id") val userId: Long,
    val username: String,
)

/**
 * Ошибки API, разделённые по тому, что с ними делать (контракт §2):
 * [Unauthorized] ретраи не лечат — нужен вход; [Network] и [Server] —
 * временные, повторяем.
 */
sealed class ApiException(message: String, cause: Throwable? = null) : Exception(message, cause) {
    /** 401/403: неверный пароль или мёртвая сессия. */
    class Unauthorized(val serverMessage: String) : ApiException("unauthorized: $serverMessage")

    /** Прочий не-2xx ответ. */
    class Server(val code: Int, val serverMessage: String) : ApiException("HTTP $code: $serverMessage")

    /** Нет соединения, таймаут, TLS. */
    class Network(cause: IOException) : ApiException(cause.message ?: cause.javaClass.simpleName, cause)
}

/** Клиент `/api/desktop/v1` сервера [baseUrl] (`https://хост`, без `/` в конце). */
class DdmailApi(private val baseUrl: String, private val http: OkHttpClient) {

    suspend fun login(username: String, password: String): LoginResponse {
        val body = json.encodeToString(LoginRequest.serializer(), LoginRequest(username, password))
        val req = Request.Builder()
            .url("$baseUrl/api/desktop/v1/auth/login")
            .post(body.toRequestBody(JSON))
            .build()
        return json.decodeFromString(LoginResponse.serializer(), http.newCall(req).awaitBody())
    }

    /** Обменять токен (в том числе просроченный, не старше 30 суток) на свежий. */
    suspend fun refresh(token: String): LoginResponse {
        val req = Request.Builder()
            .url("$baseUrl/api/desktop/v1/auth/refresh")
            .header("Authorization", "Bearer $token")
            .post(ByteArray(0).toRequestBody(null))
            .build()
        return json.decodeFromString(LoginResponse.serializer(), http.newCall(req).awaitBody())
    }

    @Serializable
    private data class LoginRequest(val username: String, val password: String)

    companion object {
        internal val json = Json {
            ignoreUnknownKeys = true
            // null вместо коллекции (nil-срез Go) — значение по умолчанию.
            coerceInputValues = true
        }
        private val JSON = "application/json; charset=utf-8".toMediaType()

        /**
         * Отвечает ли на [host] сервер ddmail: `GET https://host/.well-known/ddmail`
         * с `{"ddmail": true}`. Любая ошибка — «нет».
         */
        suspend fun isDdmail(http: OkHttpClient, host: String): Boolean {
            val req = Request.Builder().url("https://$host/.well-known/ddmail").build()
            return try {
                val body = http.newCall(req).awaitBody()
                json.parseToJsonElement(body).jsonObject["ddmail"]?.jsonPrimitive?.boolean == true
            } catch (e: Exception) {
                if (e is kotlinx.coroutines.CancellationException) throw e
                false
            }
        }
    }
}

/** Выполнить запрос; 2xx — тело, иначе [ApiException]. Отмена корутины отменяет запрос. */
internal suspend fun Call.awaitBody(): String = suspendCancellableCoroutine { cont ->
    cont.invokeOnCancellation { cancel() }
    enqueue(object : Callback {
        override fun onFailure(call: Call, e: IOException) {
            if (!cont.isCancelled) cont.resumeWithException(ApiException.Network(e))
        }

        override fun onResponse(call: Call, response: Response) {
            val result = response.use { r ->
                val text = try {
                    r.body?.string().orEmpty()
                } catch (e: IOException) {
                    return@use Result.failure(ApiException.Network(e))
                }
                when {
                    r.isSuccessful -> Result.success(text)
                    r.code == 401 || r.code == 403 -> Result.failure(ApiException.Unauthorized(errorText(text)))
                    else -> Result.failure(ApiException.Server(r.code, errorText(text)))
                }
            }
            result.fold({ cont.resume(it) }, { cont.resumeWithException(it) })
        }
    })
}

/** Сервер отвечает ошибкой `{"error": "..."}`; не JSON — текст как есть. */
private fun errorText(body: String): String = try {
    DdmailApi.json.parseToJsonElement(body).jsonObject["error"]?.jsonPrimitive?.content ?: body
} catch (_: Exception) {
    body
}.trim()
