package ru.letotam.ddmail.dav

import ru.letotam.ddmail.api.ContactDto
import ru.letotam.ddmail.api.EventDto
import ru.letotam.ddmail.imap.MailException

/** Коллекция DAV: календарь или адресная книга. */
data class DavCollection(val url: String, val name: String, val color: String, val canWrite: Boolean)

/** Событие с адресом ресурса и ETag — для правки и удаления. */
data class DavEvent(val href: String, val etag: String?, val event: EventDto)

data class DavContact(val href: String, val etag: String?, val contact: ContactDto, val uid: String)

/**
 * CalDAV/CardDAV внешней учётки (mobile-external-accounts §8) — порт
 * `caldav_client.rs`/`carddav_client.rs`, но **все** календари и книги
 * home-set, а не первый: обнаружение `current-user-principal` →
 * `calendar-home-set`/`addressbook-home-set` → коллекции (`.well-known` —
 * обычной базой). Если адрес сам — коллекция, она одна.
 */
object DavPim {
    private const val P_BASE = "<D:resourcetype/><D:current-user-principal/><D:displayname/><D:current-user-privilege-set/><A:calendar-color/><C:calendar-home-set/><CR:addressbook-home-set/>"

    /** Все коллекции нужного типа (`calendar` или `addressbook`) по базовому адресу. */
    suspend fun collections(c: DavClient, base: String, type: String): List<DavCollection> {
        val homeProp = if (type == "calendar") "calendar-home-set" else "addressbook-home-set"
        val root = c.propfind(base, 0, P_BASE).firstOrNull() ?: return emptyList()
        if (root.isType(type)) return listOf(collection(base, root))
        val home = root.hrefIn(homeProp)?.let { DavClient.absUrl(base, it) } ?: run {
            val principal = root.hrefIn("current-user-principal")?.let { DavClient.absUrl(base, it) } ?: return emptyList()
            c.propfind(principal, 0, P_BASE).firstOrNull()?.hrefIn(homeProp)?.let { DavClient.absUrl(principal, it) }
        } ?: return emptyList()
        return c.propfind(home, 1, P_BASE).filter { it.isType(type) }.map { r -> collection(DavClient.absUrl(home, r.href), r) }
    }

    private fun collection(url: String, r: DavResponse): DavCollection {
        val privs = r.prop("current-user-privilege-set")?.textContent.orEmpty()
        val writeKnown = r.prop("current-user-privilege-set") != null
        val canWrite = !writeKnown || r.prop("current-user-privilege-set")!!.getElementsByTagNameNS("DAV:", "write").length > 0 ||
            r.prop("current-user-privilege-set")!!.getElementsByTagNameNS("DAV:", "all").length > 0 || privs.contains("write")
        val color = r.text("calendar-color")?.take(7).orEmpty()
        val name = r.text("displayname") ?: url.trimEnd('/').substringAfterLast('/')
        return DavCollection(url.let { if (it.endsWith('/')) it else "$it/" }, name, color, canWrite)
    }

    /** События, пересекающие `[from, to)` (REPORT calendar-query с time-range). */
    suspend fun events(c: DavClient, url: String, fromMs: Long, toMs: Long): List<DavEvent> {
        val body = """<?xml version="1.0" encoding="utf-8"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><D:getetag/><C:calendar-data/></D:prop>
  <C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">
    <C:time-range start="${Ical.stamp(fromMs)}" end="${Ical.stamp(toMs)}"/>
  </C:comp-filter></C:comp-filter></C:filter>
</C:calendar-query>"""
        return c.report(url, body).flatMap { r ->
            val data = r.text("calendar-data") ?: return@flatMap emptyList()
            Ical.events(data).map { DavEvent(DavClient.absUrl(url, r.href), r.text("getetag"), it) }
        }
    }

    suspend fun contacts(c: DavClient, url: String): List<DavContact> {
        val body = """<?xml version="1.0" encoding="utf-8"?>
<CR:addressbook-query xmlns:D="DAV:" xmlns:CR="urn:ietf:params:xml:ns:carddav">
  <D:prop><D:getetag/><CR:address-data/></D:prop>
</CR:addressbook-query>"""
        return c.report(url, body).mapNotNull { r ->
            val data = r.text("address-data") ?: return@mapNotNull null
            val (uid, contact) = VCard.parse(data) ?: return@mapNotNull null
            DavContact(DavClient.absUrl(url, r.href), r.text("getetag"), contact, uid)
        }
    }

    fun requireWrite(col: DavCollection) {
        if (!col.canWrite) throw MailException.Rejected("Календарь или книга только для чтения")
    }
}

/** vCard 3.0 (`build_vcard`/`parse_vcard` десктопа). */
object VCard {
    fun escape(s: String) = s.replace("\\", "\\\\").replace(",", "\\,").replace(";", "\\;").replace("\n", "\\n")

    fun build(uid: String, fullName: String, emails: List<String>, phones: List<String>, org: String, title: String): String {
        val sb = StringBuilder("BEGIN:VCARD\r\nVERSION:3.0\r\n")
        sb.append("UID:").append(uid).append("\r\n")
        if (fullName.isNotEmpty()) sb.append("FN:").append(escape(fullName)).append("\r\n")
        sb.append("N:;").append(escape(fullName)).append(";;;\r\n")
        emails.filter { it.isNotBlank() }.forEach { sb.append("EMAIL:").append(escape(it.trim())).append("\r\n") }
        phones.filter { it.isNotBlank() }.forEach { sb.append("TEL:").append(escape(it.trim())).append("\r\n") }
        if (org.isNotEmpty()) sb.append("ORG:").append(escape(org)).append("\r\n")
        if (title.isNotEmpty()) sb.append("TITLE:").append(escape(title)).append("\r\n")
        sb.append("END:VCARD\r\n")
        return sb.toString()
    }

    /** UID и поля контакта; без имени и адресов — `null`. */
    fun parse(card: String): Pair<String, ContactDto>? {
        var uid = ""
        var fn = ""
        val emails = mutableListOf<String>()
        val phones = mutableListOf<String>()
        var org = ""
        var title = ""
        for (l in Ical.unfold(card)) {
            val p = Ical.prop(l) ?: continue
            val v = Ical.unescape(p.value.trim())
            if (v.isEmpty()) continue
            when (p.name.substringAfterLast('.')) {
                "UID" -> uid = v
                "FN" -> fn = v
                "EMAIL" -> emails += v
                "TEL" -> phones += v
                "ORG" -> org = p.value.split(';').first().let(Ical::unescape).trim()
                "TITLE" -> title = v
            }
        }
        if (fn.isEmpty() && emails.isEmpty()) return null
        return uid to ContactDto(id = 0, fullName = fn, emails = emails, phones = phones, organization = org, title = title)
    }
}
