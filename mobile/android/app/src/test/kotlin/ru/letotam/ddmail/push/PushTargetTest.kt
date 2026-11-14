package ru.letotam.ddmail.push

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import ru.letotam.ddmail.auth.Session

/** Какой должна быть регистрация push и когда её повторять (контракт §7). */
class PushTargetTest {
    private val session = Session("https://mail.example.org", "alice", 7, "jwt")

    @Test
    fun desiredOnlyForDdmailWithTokenAndNotifications() {
        val want = PushTarget.desired(session, "hms", "tok", notificationsAllowed = true)
        assertEquals(PushTarget("https://mail.example.org", "alice", "hms", "tok"), want)
        // Нет ddmail-учётки (только IMAP) — push не нужен.
        assertNull(PushTarget.desired(null, "hms", "tok", true))
        // Токена ещё нет.
        assertNull(PushTarget.desired(session, "hms", null, true))
        assertNull(PushTarget.desired(session, "hms", " ", true))
        // Уведомления выключены или запрещены — будить телефон незачем.
        assertNull(PushTarget.desired(session, "rustore", "tok", false))
    }

    @Test
    fun encodeDecodeRoundTrip() {
        val t = PushTarget("https://mail.example.org", "alice", "rustore", "a-b_c:d")
        assertEquals(t, PushTarget.decode(t.encode()))
        assertNull(PushTarget.decode(null))
        assertNull(PushTarget.decode("garbage"))
        assertNull(PushTarget.decode("a\nb\n\nd"))
    }

    @Test
    fun belongsToSameAccountOnly() {
        val t = PushTarget("https://mail.example.org", "alice", "hms", "tok")
        assertTrue(t.belongsTo(session))
        // Новый токен сессии (refresh) — та же учётка.
        assertTrue(t.belongsTo(session.copy(token = "other")))
        assertFalse(t.belongsTo(session.copy(username = "bob")))
        assertFalse(t.belongsTo(session.copy(serverUrl = "https://other.example.org")))
    }

    @Test
    fun registerOnChangeOrOncePerDay() {
        val t = PushTarget("s", "u", "hms", "tok")
        val day = PushTarget.REFRESH_MS
        val now = 10 * day
        assertTrue("first time", PushTarget.needsRegister(t, null, 0, now))
        assertFalse("fresh", PushTarget.needsRegister(t, t, now - 1000, now))
        assertTrue("a day old", PushTarget.needsRegister(t, t, now - day, now))
        assertTrue("token changed", PushTarget.needsRegister(t, t.copy(token = "new"), now - 1000, now))
        assertTrue("clock went back", PushTarget.needsRegister(t, t, now + 1000, now))
        assertFalse("nothing wanted", PushTarget.needsRegister(null, t, 0, now))
    }
}
