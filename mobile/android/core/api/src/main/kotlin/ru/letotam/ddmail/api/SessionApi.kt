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
 * `/auth/refresh` и повторить; refresh тоже отказал — сессия мертва,
 * [ApiException.Unauthorized] уходит наверх (контракт §2: «нужен вход»).
 * Обмен сериализован: десять запросов, разом упёршихся в 401, делают один
 * refresh, а не десять.
 */
class SessionApi(
    private val baseUrl: String,
    private val http: OkHttpClient,
    private val tokens: TokenSource,
) : PimApi {
    private val raw = DdmailApi(baseUrl, http)
    private val refreshLock = Mutex()

    suspend fun conversationsDelta(sinceMs: Long, limit: Int = 5000): ConversationsDelta =
        get("/conversations?limit=$limit&since=${sinceMs.coerceAtLeast(0)}", ConversationsDelta.serializer())

    /**
     * Беседы из писем, которые сервер счёл спамом, — всегда полный список.
     * Сервер без `spam=1` отдал бы обычный список; такие беседы спамом не
     * считаются (флага `spam` у них нет).
     */
    suspend fun spamConversations(limit: Int = 2000): List<ConversationDto> =
        get("/conversations?spam=1&limit=$limit", ListSerializer(ConversationDto.serializer())).filter { it.spam }

    /**
     * «Не спам»: письма — во «Входящие». Сколько вернулось.
     * [trust] — «Не спам и доверять отправителю»: сервер заводит allow-правило
     * на адрес отправителя и возвращает из спама всё остальное от него.
     */
    suspend fun restoreSpam(refs: List<MessageRef>, trust: Boolean = false): Int {
        val body = write("POST", "/spam/restore", restoreSpamBody(refs, trust))
        val obj = runCatching { kotlinx.serialization.json.Json.parseToJsonElement(body) as? JsonObject }.getOrNull()
        return (obj?.get("restored") as? JsonPrimitive)?.content?.toIntOrNull() ?: 0
    }

    /**
     * «В спам»: удалить письма бесед и заблокировать их отправителей.
     * Отправителя сервер берёт из самих писем ([messageIds] — серверные id,
     * то есть `uid` ссылок), а не из догадки клиента: в BCC-рассылке среди
     * участников беседы есть и получатель. [scope] — `address` или `domain`
     * (против спамеров с меняющейся частью до `@`). [fallbackAddress] — только
     * на случай, если по id сервер отправителя не нашёл.
     */
    suspend fun blacklistAndPurge(scope: String, messageIds: List<Long>, fallbackAddress: String): PurgeOutcome {
        val body = write("POST", "/spam/blacklist-and-purge", blacklistAndPurgeBody(scope, messageIds, fallbackAddress))
        return DdmailApi.json.decodeFromString(PurgeOutcome.serializer(), body)
    }

    suspend fun changes(since: Long): ChangesResponse =
        get("/changes?since=${since.coerceAtLeast(0)}", ChangesResponse.serializer())

    suspend fun dialogFolders(): DialogFoldersResponse =
        get("/dialog-folders", DialogFoldersResponse.serializer())

    /** Группы своих адресов (цвета строк, подписи) — тот же формат `version`+`doc`. */
    suspend fun identityGroups(): DialogFoldersResponse =
        get("/identity-groups", DialogFoldersResponse.serializer())

    /**
     * Пуш-канал сервера (`/ws`). Токен — заголовком (контракт §2) и, пока
     * сервер читает только его, ещё и в query; в логи клиента URL не пишется.
     * [onEvent] получает тип события; [onClosed] — `true`, если сервер отверг
     * токен (тогда сначала обменять его, потом переподключаться).
     */
    suspend fun openSocket(onEvent: (String) -> Unit, onClosed: (authFailed: Boolean) -> Unit): okhttp3.WebSocket {
        val token = tokens.current()
        val wsUrl = baseUrl.replaceFirst("https://", "wss://") + "/api/desktop/v1/ws?token=" +
            java.net.URLEncoder.encode(token, "UTF-8")
        val req = Request.Builder().url(wsUrl).header("Authorization", "Bearer $token").build()
        return http.newWebSocket(req, object : okhttp3.WebSocketListener() {
            override fun onMessage(webSocket: okhttp3.WebSocket, text: String) {
                val type = runCatching {
                    (DdmailApi.json.parseToJsonElement(text) as JsonObject)["type"]
                        ?.let { (it as JsonPrimitive).content }
                }.getOrNull() ?: return
                onEvent(type)
            }

            override fun onFailure(webSocket: okhttp3.WebSocket, t: Throwable, response: Response?) {
                onClosed(response?.code == 401 || response?.code == 403)
            }

            override fun onClosed(webSocket: okhttp3.WebSocket, code: Int, reason: String) {
                onClosed(false)
            }
        })
    }

    // ── Календарь, задачи, контакты ──

    override suspend fun calendars(): List<CalendarDto> = get("/calendars", ListSerializer(CalendarDto.serializer()))

    /** События окна `[fromMs, toMs)`; повторяющиеся — мастерами (развёртывает клиент). */
    override suspend fun calendarEvents(fromMs: Long, toMs: Long): List<EventDto> =
        get("/calendar-events?from=$fromMs&to=$toMs", ListSerializer(EventDto.serializer()))

    override suspend fun createEvent(req: EventCreateRequest): String =
        write("POST", "/events", DdmailApi.json.encodeToString(EventCreateRequest.serializer(), req))

    override suspend fun patchEvent(id: Long, req: EventPatchRequest): String =
        write("PATCH", "/events/$id", DdmailApi.json.encodeToString(EventPatchRequest.serializer(), req))

    override suspend fun deleteEvent(id: Long) {
        write("DELETE", "/events/$id", null)
    }

    /** Ответ на приглашение: ACCEPTED | TENTATIVE | DECLINED. */
    override suspend fun rsvp(id: Long, partstat: String) {
        write("POST", "/events/$id/rsvp", JsonObject(mapOf("partstat" to JsonPrimitive(partstat))).toString())
    }

    suspend fun tasks(includeCompleted: Boolean): List<TaskDto> =
        get("/tasks" + if (includeCompleted) "?include_completed=1" else "", ListSerializer(TaskDto.serializer()))

    suspend fun setTaskCompleted(id: Long, completed: Boolean) {
        write("POST", "/tasks/$id/completion", JsonObject(mapOf("completed" to JsonPrimitive(completed))).toString())
    }

    override suspend fun contacts(limit: Int): List<ContactDto> =
        get("/contacts?limit=$limit", ListSerializer(ContactDto.serializer()))

    override suspend fun addressBooks(): List<AddressBookDto> =
        get("/address-books", ListSerializer(AddressBookDto.serializer()))

    override suspend fun createContact(req: ContactWriteRequest) {
        write("POST", "/contacts", DdmailApi.json.encodeToString(ContactWriteRequest.serializer(), req))
    }

    override suspend fun patchContact(id: Long, req: ContactWriteRequest) {
        write("PATCH", "/contacts/$id", DdmailApi.json.encodeToString(ContactWriteRequest.serializer(), req))
    }

    override suspend fun deleteContact(id: Long) {
        write("DELETE", "/contacts/$id", null)
    }

    private suspend fun write(method: String, path: String, json: String?): String = authed { token ->
        Request.Builder().url("$baseUrl/api/desktop/v1$path")
            .header("Authorization", "Bearer $token")
            .method(method, json?.toRequestBody(JSON))
            .build()
    }

    override suspend fun identities(): List<Identity> =
        get("/identities", ListSerializer(Identity.serializer()))

    /** Поставить письмо в отправку. 413 — вложения больше лимита сервера. */
    suspend fun send(req: SendRequest) {
        val body = DdmailApi.json.encodeToString(SendRequest.serializer(), req)
        authed { token ->
            Request.Builder().url("$baseUrl/api/desktop/v1/send")
                .header("Authorization", "Bearer $token")
                .post(body.toRequestBody(JSON))
                .build()
        }
    }

    /** Мягкое удаление писем (vault на сервере, у внешних учёток — и на источнике). */
    suspend fun delete(refs: List<MessageRef>) {
        val msgs = JsonArray(refs.map { JsonObject(mapOf("folder" to JsonPrimitive(it.folder), "uid" to JsonPrimitive(it.uid), "message_id" to JsonPrimitive(it.messageId))) })
        authed { token ->
            Request.Builder().url("$baseUrl/api/desktop/v1/messages/delete")
                .header("Authorization", "Bearer $token")
                .post(JsonObject(mapOf("messages" to msgs)).toString().toRequestBody(JSON))
                .build()
        }
    }

    /** Документ папок: `409` — его поменяла другая станция, в ответе текущий. */
    suspend fun saveDialogFolders(baseVersion: Long, doc: kotlinx.serialization.json.JsonElement): DialogFoldersResponse {
        val body = JsonObject(mapOf("base_version" to JsonPrimitive(baseVersion), "doc" to doc)).toString()
        val text = authed { token ->
            Request.Builder().url("$baseUrl/api/desktop/v1/dialog-folders")
                .header("Authorization", "Bearer $token")
                .put(body.toRequestBody(JSON))
                .build()
        }
        return DdmailApi.json.decodeFromString(DialogFoldersResponse.serializer(), text)
    }

    /** Аватар адреса (`/avatars?email=`): байты картинки или `null`, если нет. */
    suspend fun avatar(email: String): Binary? = try {
        // 204 — аватара нет: пустое тело.
        binary("/avatars?email=" + java.net.URLEncoder.encode(email, "UTF-8")).takeIf { it.bytes.isNotEmpty() }
    } catch (e: ApiException.Server) {
        null
    }

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
                        r.code == 401 -> Result.failure(ApiException.Unauthorized(""))
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

/** Внешний двоичный ресурс (Gravatar): 404 — `null`, прочие отказы — [ApiException]. */
suspend fun fetchBinary(http: OkHttpClient, req: Request): Binary? = try {
    http.newCall(req).awaitBinary()
} catch (e: ApiException.Server) {
    if (e.code == 404) null else throw e
}

/** GET текста (автонастройка): тело при 2xx, иначе `null`. */
suspend fun fetchText(http: OkHttpClient, url: String): String? = try {
    http.newCall(Request.Builder().url(url).build()).awaitBody()
} catch (e: ApiException) {
    null
}

/** Тело `POST /spam/restore`; `trust` шлётся только когда он нужен. */
internal fun restoreSpamBody(refs: List<MessageRef>, trust: Boolean): String {
    val msgs = JsonArray(refs.map { JsonObject(mapOf("uid" to JsonPrimitive(it.uid), "message_id" to JsonPrimitive(it.messageId))) })
    val fields = buildMap {
        put("messages", msgs)
        if (trust) put("trust", JsonPrimitive(true))
    }
    return JsonObject(fields).toString()
}

/** Тело `POST /spam/blacklist-and-purge` — как у десктопа (`native_provider.rs`). */
internal fun blacklistAndPurgeBody(scope: String, messageIds: List<Long>, fallbackAddress: String): String =
    JsonObject(
        mapOf(
            "scope" to JsonPrimitive(scope),
            "address" to JsonPrimitive(fallbackAddress),
            "message_ids" to JsonArray(messageIds.map { JsonPrimitive(it) }),
        ),
    ).toString()
