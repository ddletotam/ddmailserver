package ru.letotam.ddmail.sync

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import ru.letotam.ddmail.api.ContactInfo

/** Те же случаи, что `folders.rs` десктопа: смысл документа обязан совпадать. */
class DialogFoldersTest {
    private fun conv(id: String, receivedBy: String, cps: List<String>, group: Boolean = false, unread: Int = 0) =
        Conversation(
            accountKey = "", id = id, label = id, receivedBy = receivedBy,
            counterparts = cps.map { ContactInfo(addr = it) }, isGroup = group,
            lastDateTs = 0, lastSubject = "", unreadCount = unread, totalCount = 1, messages = emptyList(),
        )

    private fun folder(id: String, rules: FolderRules = FolderRules(), include: List<DialogKey> = emptyList(), exclude: List<DialogKey> = emptyList()) =
        DialogFolder(id, id, "", rules, include, exclude, hidden = false, autoRead = false)

    private fun key(id: String) = DialogKey("", id)

    @Test
    fun noRulesNoIncludeMatchesNothing() {
        val doc = FoldersDoc(listOf(folder("a")))
        assertNull(doc.folderOf(conv("x", "me@a", listOf("bob@b"))))
    }

    @Test
    fun rulesAndBetweenCategoriesOrWithin() {
        val doc = FoldersDoc(listOf(folder("a", FolderRules(identities = listOf("me@a", "me@c"), domains = listOf("acme.com")))))
        assertEquals(0, doc.folderOf(conv("1", "me@a", listOf("bob@mail.acme.com"))))
        assertEquals(0, doc.folderOf(conv("2", "ME@C", listOf("x@acme.com"))))
        assertNull(doc.folderOf(conv("3", "me@a", listOf("bob@notacme.com"))))
        assertNull(doc.folderOf(conv("4", "me@z", listOf("bob@acme.com"))))
    }

    @Test
    fun kindRule() {
        val doc = FoldersDoc(listOf(folder("g", FolderRules(kind = DialogKind.Group))))
        assertEquals(0, doc.folderOf(conv("1", "me", listOf("a", "b"), group = true)))
        assertNull(doc.folderOf(conv("2", "me", listOf("a"))))
    }

    @Test
    fun firstMatchingFolderWinsIncludeBeatsRules() {
        val r = FolderRules(domains = listOf("acme.com"))
        val c = conv("1", "me", listOf("bob@acme.com"))
        assertEquals(0, FoldersDoc(listOf(folder("a", r), folder("b", r))).folderOf(c))
        val doc = FoldersDoc(listOf(folder("a", r), folder("b", r), folder("c", include = listOf(key("1")))))
        assertEquals(2, doc.folderOf(c))
    }

    @Test
    fun excludeDropsToNextMatchingOrInbox() {
        val r = FolderRules(domains = listOf("acme.com"))
        val c = conv("1", "me", listOf("bob@acme.com"))
        assertEquals(1, FoldersDoc(listOf(folder("a", r, exclude = listOf(key("1"))), folder("b", r))).folderOf(c))
        assertNull(FoldersDoc(listOf(folder("a", r, exclude = listOf(key("1"))))).folderOf(c))
    }

    @Test
    fun assignMovesStrictlyAndToInbox() {
        val r = FolderRules(domains = listOf("acme.com"))
        val c = conv("1", "me", listOf("bob@acme.com"))
        var doc = FoldersDoc(listOf(folder("a", r), folder("b")))
        assertEquals(0, doc.folderOf(c))
        doc = doc.assign(listOf(c), 1)
        assertEquals(1, doc.folderOf(c))
        doc = doc.assign(listOf(c), null)
        assertNull("в инбокс, хотя правила папки a его ловят", doc.folderOf(c))
        doc = doc.assign(listOf(c), 0)
        assertEquals(0, doc.folderOf(c))
        assertEquals(1, doc.folders[0].include.size)
        assertTrue(doc.folders[0].exclude.isEmpty())
    }

    @Test
    fun bulkAssignIsOneEdit() {
        val cs = listOf(conv("1", "me", listOf("a@x")), conv("2", "me", listOf("b@y")))
        val doc = FoldersDoc(listOf(folder("a"))).assign(cs, 0)
        assertEquals(listOf(key("1"), key("2")), doc.folders[0].include)
    }

    @Test
    fun filterAndCounts() {
        val doc = FoldersDoc(listOf(folder("f", FolderRules(domains = listOf("acme.com")))))
        val all = listOf(
            conv("a", "me", listOf("x@acme.com"), unread = 2),
            conv("b", "me", listOf("y@acme.com")),
            conv("g", "me", listOf("z@other.ru"), unread = 1),
        )
        assertEquals(listOf("a", "b"), doc.filter(all, 0).map { it.id })
        assertEquals(listOf("g"), doc.filter(all, null).map { it.id })
        assertEquals(listOf(1) to 1, doc.unreadCounts(all))
    }

    @Test
    fun unknownFieldsSurviveRoundtrip() {
        val src = """{"schema":2,"folders":[{"id":"f","name":"F","color":"#fff",
            "rules":{"kind":"group","future":1},"include":[{"account":"u@h","id":"a|b"}]}]}"""
        val doc = FoldersDoc.parse(Json.parseToJsonElement(src))
        assertEquals(DialogKind.Group, doc.folders[0].rules.kind)
        assertEquals(DialogKey("u@h", "a|b"), doc.folders[0].include.single())
        val back = doc.toJson()
        assertEquals(2, back["schema"]!!.jsonPrimitive.int)
        val f = back["folders"]!!.jsonArray[0].jsonObject
        assertEquals("#fff", f["color"]!!.jsonPrimitive.content)
        assertEquals(1, f["rules"]!!.jsonObject["future"]!!.jsonPrimitive.int)
    }

    // Папка «Спам» — те же случаи, что `spam_lives_only_in_system_folder`
    // и `ensure_spam_is_idempotent_and_repairs` десктопа.
    @Test
    fun spamLivesOnlyInSystemFolder() {
        val anyA = FolderRules(identities = listOf("me@a"))
        val bare = FoldersDoc(listOf(folder("a", anyA)))
        val spam = conv("spam:me@a|bad@x", "me@a", listOf("bad@x"), unread = 1).copy(spam = true)
        val ham = conv("me@a|bob@b", "me@a", listOf("bob@b"), unread = 1)
        val all = listOf(spam, ham)

        // Без папки «Спам» спама не видно нигде, даже если правила его ловят.
        assertNull(bare.folderOf(spam))
        assertEquals(emptyList<String>(), bare.filter(all, null).map { it.id })
        assertEquals(listOf(1) to 0, bare.unreadCounts(all))

        val doc = bare.withSpam()
        val si = doc.spamIndex!!
        assertTrue(doc.folders[si].hidden)
        assertEquals(si, doc.folderOf(spam))
        assertEquals(0, doc.folderOf(ham))
        assertEquals(listOf("spam:me@a|bad@x"), doc.filter(all, si).map { it.id })
        assertEquals(listOf(1, 1) to 0, doc.unreadCounts(all))

        // Руками ни внутрь, ни наружу.
        assertEquals(0, doc.assign(listOf(ham), si).folderOf(ham))
        assertEquals(si, doc.assign(listOf(spam), null).folderOf(spam))
    }

    /**
     * Документ с сервера (или локальный), где другой клиент снял «скрыта» с
     * «Спама», — после чтения папка снова скрыта, и в JSON тоже.
     */
    @Test
    fun normalizeRepairsUnhiddenSpamFromJson() {
        val text = """{"folders":[{"id":"spam","name":"Спам","icon":"🚫","rules":{},"hidden":false,""" +
            """"auto_read":true,"system":"spam","x":1}],"v":2}"""
        val doc = FoldersDoc.parse(Json.parseToJsonElement(text)).withSpam()
        val f = doc.folders.single()
        assertTrue(f.hidden)
        assertTrue(!f.autoRead)
        val out = doc.toJson()["folders"]!!.jsonArray[0].jsonObject
        assertEquals("true", out["hidden"].toString())
        assertEquals(1, out["x"]!!.jsonPrimitive.int)
        assertEquals(1, doc.hiddenCount)
    }

    /** Таб «Спама» — только по «Показать скрытые» или когда в него привели. */
    @Test
    fun spamTabHiddenUnlessShowHiddenOrActive() {
        val doc = FoldersDoc(listOf(folder("a"))).withSpam()
        val si = doc.spamIndex!!
        assertTrue(!doc.tabVisible(si, showHidden = false, active = null))
        assertTrue(!doc.tabVisible(si, showHidden = false, active = 0))
        assertTrue(doc.tabVisible(si, showHidden = true, active = null))
        assertTrue(doc.tabVisible(si, showHidden = false, active = si))
        assertTrue(doc.tabVisible(0, showHidden = false, active = null))
        assertEquals(1, doc.hiddenCount)
    }

    @Test
    fun withSpamIsIdempotentRepairsAndRoundtrips() {
        val once = FoldersDoc().withSpam()
        assertEquals(once.folders, once.withSpam().folders)
        // Другой клиент снял «скрыта» и задублировал — чиним.
        val broken = once.copy(folders = once.folders.map { it.copy(hidden = false) } + once.folders)
        val fixed = broken.withSpam()
        assertEquals(1, fixed.folders.size)
        assertTrue(fixed.folders[0].hidden)
        // Через JSON — та же системная папка.
        val back = FoldersDoc.parse(Json.parseToJsonElement(once.toJson().toString()))
        assertEquals(SYSTEM_SPAM, back.folders[0].system)
        assertEquals(0, back.spamIndex)
        // Пользовательская папка с id "spam" не мешает.
        val clash = FoldersDoc(listOf(folder("spam"))).withSpam()
        assertEquals(2, clash.folders.size)
        assertTrue(clash.folders[1].id != "spam")
        assertNull(clash.folderOf(conv("x", "me@a", listOf("x@y"))))
    }

    @Test
    fun editorFieldsRoundtripAndUnknownSurvive() {
        val src = """{"x":1,"folders":[{"id":"a","name":"A","icon":"","rules":{"domains":["acme.com"],"future":true},"later":"keep"}]}"""
        val doc = FoldersDoc.parse(Json.parseToJsonElement(src))
        val edited = doc.edited("a") {
            it.copy(
                name = "Работа", icon = "💼",
                rules = it.rules.copy(identities = splitRuleList("Me@A.ru, me@b.ru"), kind = DialogKind.Direct),
                hidden = true, autoRead = true,
            )
        }
        val json = edited.toJson()
        val back = FoldersDoc.parse(Json.parseToJsonElement(json.toString()))
        val f = back.folders.single()
        assertEquals("Работа", f.name)
        assertEquals("💼", f.icon)
        assertEquals(listOf("me@a.ru", "me@b.ru"), f.rules.identities)
        assertEquals(listOf("acme.com"), f.rules.domains)
        assertEquals(DialogKind.Direct, f.rules.kind)
        assertTrue(f.hidden && f.autoRead)
        // Неизвестные поля документа, папки и правил переживают правку.
        assertEquals(1, json["x"]!!.jsonPrimitive.int)
        val fo = json["folders"]!!.jsonArray[0].jsonObject
        assertEquals("keep", fo["later"]!!.jsonPrimitive.content)
        assertTrue(fo["rules"]!!.jsonObject["future"]!!.jsonPrimitive.content == "true")
        // У обычной папки `system` не пишется.
        assertNull(fo["system"])
    }

    @Test
    fun addMoveRemove() {
        val doc = FoldersDoc().withSpam().added("f1").added("f2")
        assertEquals(listOf("spam", "f1", "f2"), doc.folders.map { it.id })
        assertEquals("Папка 2", doc.folders[1].name)
        assertEquals("📁", doc.folders[1].icon)
        assertEquals(listOf("f2", "spam", "f1"), doc.moved("f2", "spam").folders.map { it.id })
        assertEquals(listOf("spam", "f2"), doc.removed("f1").folders.map { it.id })
        // Системную не удалить и не настроить.
        assertEquals(3, doc.removed("spam").folders.size)
        assertEquals("Спам", doc.edited("spam") { it.copy(name = "x") }.folders[0].name)
        val id = doc.newId(0x10)
        assertTrue(id.startsWith("f10"))
    }
}
