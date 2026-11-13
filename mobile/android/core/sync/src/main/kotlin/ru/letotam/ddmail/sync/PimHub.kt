package ru.letotam.ddmail.sync

import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import okhttp3.OkHttpClient
import ru.letotam.ddmail.account.ImapAccount
import ru.letotam.ddmail.api.AddressBookDto
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.api.CalendarDto
import ru.letotam.ddmail.api.ContactDto
import ru.letotam.ddmail.api.ContactWriteRequest
import ru.letotam.ddmail.api.EventCreateRequest
import ru.letotam.ddmail.api.EventDto
import ru.letotam.ddmail.api.EventPatchRequest
import ru.letotam.ddmail.api.Identity
import ru.letotam.ddmail.api.PimApi
import ru.letotam.ddmail.dav.DavClient
import ru.letotam.ddmail.dav.DavCollection
import ru.letotam.ddmail.dav.DavPim
import ru.letotam.ddmail.dav.Ical
import ru.letotam.ddmail.dav.Precondition
import ru.letotam.ddmail.dav.VCard
import ru.letotam.ddmail.imap.MailException
import java.util.UUID

/**
 * Общий календарь и контакты (mobile-external-accounts §8): сервер ddmail и
 * CalDAV/CardDAV внешних учёток вместе. Элементы внешних учёток получают
 * отрицательные id (хеш учётки и ресурса): с положительными id сервера они
 * не пересекаются, и по знаку видно, куда идёт правка. Сбой одной учётки
 * или коллекции не прячет остальные; отказ входа — «нужен вход» этой
 * учётки ([onAuthFailed]).
 */
class PimHub(
    private val ddmail: PimApi?,
    private val accounts: List<ImapAccount>,
    private val password: suspend (String) -> String?,
    private val http: OkHttpClient,
    private val onAuthFailed: (String) -> Unit,
) : PimApi {
    private class Col(val account: ImapAccount, val col: DavCollection)
    private class EvRef(val col: Col, val href: String, val etag: String?)
    private class CtRef(val col: Col, val href: String, val etag: String?, val uid: String)

    private val lock = Mutex()
    private val calendars = HashMap<Long, Col>()
    private val books = HashMap<Long, Col>()
    private val events = HashMap<Long, EvRef>()
    private val contactRefs = HashMap<Long, CtRef>()
    private var calLoaded = false
    private var bookLoaded = false

    private suspend fun client(a: ImapAccount): DavClient? {
        val pw = password(a.key) ?: return null
        return DavClient(http, a.login, pw)
    }

    /** Ошибка внешней учётки: вход — в статус, прочее — молча (покажем остальное). */
    private fun noted(a: ImapAccount, e: Exception) {
        if (e is MailException.Auth) onAuthFailed(a.key)
    }

    private suspend fun ensureCalendars() {
        if (lock.withLock { calLoaded }) return
        loadCalendars()
    }

    private suspend fun loadCalendars(): List<Col> = coroutineScope {
        val found = accounts.filter { it.caldavUrl.isNotBlank() }.map { a ->
            async {
                try {
                    val c = client(a) ?: return@async emptyList()
                    DavPim.collections(c, a.caldavUrl, "calendar").map { Col(a, it) }
                } catch (e: MailException) {
                    noted(a, e)
                    emptyList()
                }
            }
        }.awaitAll().flatten()
        lock.withLock {
            calendars.clear()
            found.forEach { calendars[negId(it.account.key + "\n" + it.col.url)] = it }
            calLoaded = true
        }
        found
    }

    private suspend fun ensureBooks() {
        if (lock.withLock { bookLoaded }) return
        loadBooks()
    }

    private suspend fun loadBooks(): List<Col> = coroutineScope {
        val found = accounts.filter { it.carddavUrl.isNotBlank() }.map { a ->
            async {
                try {
                    val c = client(a) ?: return@async emptyList()
                    DavPim.collections(c, a.carddavUrl, "addressbook").map { Col(a, it) }
                } catch (e: MailException) {
                    noted(a, e)
                    emptyList()
                }
            }
        }.awaitAll().flatten()
        lock.withLock {
            books.clear()
            found.forEach { books[negId(it.account.key + "\n" + it.col.url)] = it }
            bookLoaded = true
        }
        found
    }

    override suspend fun identities(): List<Identity> {
        val own = accounts.map { Identity(it.email.lowercase(), it.name) }
        val server = ddmail?.let { runCatching { it.identities() }.getOrNull() }.orEmpty()
        return (server + own).distinctBy { it.email.lowercase() }
    }

    override suspend fun calendars(): List<CalendarDto> {
        val server = ddmail?.calendars().orEmpty()
        loadCalendars()
        val dav = lock.withLock { calendars.entries.toList() }.map { (id, c) ->
            CalendarDto(
                id = id,
                name = c.col.name.ifBlank { c.account.email },
                description = c.account.email,
                color = c.col.color.ifBlank { "#3a6df0" },
                sourceType = "caldav",
                canWrite = c.col.canWrite,
                enabled = true,
            )
        }
        return server + dav
    }

    override suspend fun calendarEvents(fromMs: Long, toMs: Long): List<EventDto> = coroutineScope {
        val server = async { ddmail?.calendarEvents(fromMs, toMs).orEmpty() }
        ensureCalendars()
        val cols = lock.withLock { calendars.entries.toList() }
        val dav = cols.map { (calId, c) ->
            async {
                try {
                    val client = client(c.account) ?: return@async emptyList()
                    DavPim.events(client, c.col.url, fromMs, toMs).map { e ->
                        val id = negId(c.account.key + "\n" + c.col.url + "\n" + e.event.uid + "\n" + e.event.recurrenceId)
                        lock.withLock { events[id] = EvRef(c, e.href, e.etag) }
                        e.event.copy(
                            id = id,
                            calendarId = calId,
                            editable = c.col.canWrite,
                            deletable = c.col.canWrite,
                            identityEmail = c.account.email.lowercase(),
                        )
                    }
                } catch (e: MailException) {
                    noted(c.account, e)
                    emptyList()
                }
            }
        }.awaitAll().flatten()
        server.await() + dav
    }

    /** Ошибку внешнего сервера — в форму текстом, как отказ сервера ddmail. */
    private suspend fun <T> write(a: ImapAccount, block: suspend (DavClient) -> T): T {
        val c = client(a) ?: throw ApiException.Server(0, "Нет пароля учётки ${a.email}")
        return try {
            block(c)
        } catch (e: MailException) {
            noted(a, e)
            throw ApiException.Server(if (e is MailException.Auth) 401 else 0, e.message ?: "Ошибка CalDAV/CardDAV")
        }
    }

    override suspend fun createEvent(req: EventCreateRequest): String {
        if (req.calendarId >= 0) return ddmail?.createEvent(req) ?: noServer()
        ensureCalendars()
        val c = lock.withLock { calendars[req.calendarId] } ?: throw ApiException.Server(404, "Календарь не найден — обновите")
        val uid = UUID.randomUUID().toString() + "@ddmail"
        val ics = Ical.build(uid, req.summary, req.description, req.location, req.dtstart, req.dtend, req.allDay)
        write(c.account) { it.put(c.col.url + uid + ".ics", ics, "text/calendar; charset=utf-8", Precondition.IfNew) }
        return ""
    }

    override suspend fun patchEvent(id: Long, req: EventPatchRequest): String {
        if (id >= 0) return ddmail?.patchEvent(id, req) ?: noServer()
        val r = lock.withLock { events[id] } ?: throw ApiException.Server(404, "Событие не найдено — обновите календарь")
        if (req.scope == "future") throw ApiException.Server(400, "Во внешнем календаре правится вся серия: выберите «все повторы»")
        write(r.col.account) { c ->
            // Изъять-слить-положить: повтор, участники и будильники сохраняются;
            // ETag защищает от чужой правки между чтением и записью.
            val (raw, etag) = c.get(r.href)
            val merged = Ical.merge(raw, req.summary, req.description, req.location, req.dtstart, req.dtend, req.allDay)
            c.put(r.href, merged, "text/calendar; charset=utf-8", (etag ?: r.etag)?.let { Precondition.IfMatch(it) } ?: Precondition.None)
        }
        return ""
    }

    override suspend fun deleteEvent(id: Long) {
        if (id >= 0) return ddmail?.deleteEvent(id) ?: noServer()
        val r = lock.withLock { events[id] } ?: throw ApiException.Server(404, "Событие не найдено — обновите календарь")
        write(r.col.account) { c ->
            DavPim.requireWrite(r.col.col)
            val etag = runCatching { c.get(r.href).second }.getOrNull() ?: r.etag
            c.delete(r.href, etag)
        }
    }

    override suspend fun rsvp(id: Long, partstat: String) {
        if (id >= 0) return ddmail?.rsvp(id, partstat) ?: noServer()
        // Как у десктопа: ответ на приглашение уходит письмом iTIP — это умеет только сервер ddmail.
        throw ApiException.Server(400, "Ответ на приглашение во внешнем календаре — в его собственном приложении")
    }

    override suspend fun contacts(limit: Int): List<ContactDto> = coroutineScope {
        val server = async { ddmail?.contacts(limit).orEmpty() }
        ensureBooks()
        val cols = lock.withLock { books.entries.toList() }
        val dav = cols.map { (bookId, c) ->
            async {
                try {
                    val client = client(c.account) ?: return@async emptyList()
                    DavPim.contacts(client, c.col.url).map { ct ->
                        val id = negId(c.account.key + "\n" + c.col.url + "\n" + ct.href)
                        lock.withLock { contactRefs[id] = CtRef(c, ct.href, ct.etag, ct.uid) }
                        // Права — у книги (`current-user-privilege-set`): только чтение — без правки (§15).
                        ct.contact.copy(id = id, addressBookId = bookId, canWrite = c.col.canWrite)
                    }
                } catch (e: MailException) {
                    noted(c.account, e)
                    emptyList()
                }
            }
        }.awaitAll().flatten()
        server.await() + dav
    }

    override suspend fun addressBooks(): List<AddressBookDto> {
        val server = ddmail?.let { runCatching { it.addressBooks() }.getOrNull() }.orEmpty()
        loadBooks()
        return server + lock.withLock { books.entries.toList() }.map { (id, c) ->
            AddressBookDto(id, c.col.name, c.account.email, "carddav", c.account.email.lowercase(), c.col.canWrite)
        }
    }

    override suspend fun createContact(req: ContactWriteRequest) {
        if (req.addressBookId >= 0) return ddmail?.createContact(req) ?: noServer()
        ensureBooks()
        val c = lock.withLock { books[req.addressBookId] } ?: throw ApiException.Server(404, "Адресная книга не найдена — обновите")
        val uid = UUID.randomUUID().toString()
        val card = VCard.build(uid, req.fullName, req.emails, req.phones, req.organization, req.title)
        write(c.account) { it.put(c.col.url + uid + ".vcf", card, "text/vcard; charset=utf-8", Precondition.IfNew) }
    }

    override suspend fun patchContact(id: Long, req: ContactWriteRequest) {
        if (id >= 0) return ddmail?.patchContact(id, req) ?: noServer()
        val r = lock.withLock { contactRefs[id] } ?: throw ApiException.Server(404, "Контакт не найден — обновите")
        write(r.col.account) { c ->
            DavPim.requireWrite(r.col.col)
            // Правка по строкам (§15): фото, дни рождения, адреса, заметки и
            // X-свойства карточки остаются; ETag — против чужой правки между
            // чтением и записью. Пересборка из полей формы стирала всё это.
            val (raw, etag) = c.get(r.href)
            val patch = ru.letotam.ddmail.dav.VCardEdit.Patch(req.fullName, req.emails, req.phones, req.organization, req.title)
            val edited = try {
                ru.letotam.ddmail.dav.VCardEdit.apply(raw, patch)
            } catch (e: ru.letotam.ddmail.dav.VCardEdit.NoVCard) {
                throw MailException.Rejected("Карточка контакта на сервере повреждена — правка не сохранена")
            }
            if (edited.changed.isEmpty()) return@write
            c.put(r.href, edited.data, "text/vcard; charset=utf-8", (etag ?: r.etag)?.let { Precondition.IfMatch(it) } ?: Precondition.None)
        }
    }

    override suspend fun deleteContact(id: Long) {
        if (id >= 0) return ddmail?.deleteContact(id) ?: noServer()
        val r = lock.withLock { contactRefs[id] } ?: throw ApiException.Server(404, "Контакт не найден — обновите")
        write(r.col.account) { c ->
            DavPim.requireWrite(r.col.col)
            val etag = runCatching { c.get(r.href).second }.getOrNull() ?: r.etag
            c.delete(r.href, etag)
        }
    }

    private fun noServer(): Nothing = throw ApiException.Server(400, "Нет сервера ddmail")

    companion object {
        /** Устойчивый отрицательный id (FNV-1a, как `event_id_from_uid` десктопа, со знаком минус). */
        fun negId(s: String): Long {
            var h = -0x340d631b7bdddcdbL
            for (b in s.toByteArray()) {
                h = h xor (b.toLong() and 0xff)
                h *= 0x100000001b3L
            }
            return -((h ushr 2) + 1)
        }

        /**
         * Адрес CalDAV/CardDAV по домену (контракт §8: находятся автоматически
         * при добавлении учётки): `.well-known` на хостах учётки, вход — её паролем.
         */
        suspend fun discover(http: OkHttpClient, a: ImapAccount, password: String, kind: String): String? {
            val domain = a.email.substringAfterLast('@')
            val hosts = listOf(a.imapHost, domain, "$kind.$domain", a.imapHost.removePrefix("imap.").removePrefix("mail.")).distinct()
            val c = DavClient(http, a.login, password)
            val type = if (kind == "caldav") "calendar" else "addressbook"
            // Разом, а побеждает первый по порядку (как автонастройка почты).
            return coroutineScope {
                hosts.flatMap { h -> listOf("https://$h/.well-known/$kind", "https://$h/$kind/") }.map { url ->
                    async { url.takeIf { runCatching { DavPim.collections(c, url, type).isNotEmpty() }.getOrDefault(false) } }
                }.awaitAll().firstOrNull { it != null }
            }
        }
    }
}
