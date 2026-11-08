package ru.letotam.ddmail.auth

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.async
import kotlinx.coroutines.coroutineScope
import ru.letotam.ddmail.api.emailDomain
import java.util.concurrent.atomic.AtomicInteger

/** Шаг поиска для полосы прогресса: сделано из верхней границы числа проверок. */
data class DiscoveryProgress(val done: Int, val total: Int, val stage: String)

/**
 * Поиск сервера ddmail по адресу почты (контракт §3, логика — как у
 * десктопного `ddmail_core::discover`, только его ddmail-часть):
 *
 * 1. разом — MX домена и `/.well-known/ddmail` на `mail.<домен>` и `<домен>`;
 * 2. если там нет — то же на MX-хостах (не больше [MAX_MX]).
 *
 * Проверки идут параллельно, но побеждает первая успешная **по порядку**, и
 * ждём только тех, кто стоит раньше неё. Прогресс — от верхней границы,
 * известной заранее; несостоявшиеся проверки засчитываются сразу, так что
 * полоса доходит до конца при любом исходе. Пароль здесь не участвует.
 *
 * [probe] и [mx] внедряются — в тестах подменяются.
 */
class ServerDiscovery(
    private val probe: suspend (host: String) -> Boolean,
    private val mx: suspend (domain: String) -> List<String>,
) {
    /** `https://хост` найденного сервера или `null`. */
    suspend fun find(email: String, onProgress: (DiscoveryProgress) -> Unit): String? = coroutineScope {
        val done = AtomicInteger(0)
        var stage = ""
        fun emit() = onProgress(DiscoveryProgress(minOf(done.get(), TOTAL), TOTAL, stage))
        fun tick(n: Int = 1) {
            if (n > 0) {
                done.addAndGet(n)
                emit()
            }
        }

        val domain = emailDomain(email) ?: run {
            done.set(TOTAL)
            emit()
            return@coroutineScope null
        }
        stage = "Ищем сервер для $domain…"
        emit()

        val direct = listOf("mail.$domain", domain)
        val mxJob = async {
            mx(domain).take(MAX_MX).also { tick() }
        }
        val hit = firstInOrder(direct, ::tick)
        if (hit != null) {
            mxJob.cancel()
            done.set(TOTAL)
            emit()
            return@coroutineScope "https://$hit"
        }

        val mxHosts = mxJob.await().filter { it !in direct }
        tick(MAX_MX - mxHosts.size)
        if (mxHosts.isNotEmpty()) {
            stage = "Проверяем почтовые серверы домена…"
            emit()
        }
        val mxHit = firstInOrder(mxHosts, ::tick)
        done.set(TOTAL)
        emit()
        mxHit?.let { "https://$it" }
    }

    /** Все проверки разом; ответ — первый успешный по порядку списка. */
    private suspend fun CoroutineScope.firstInOrder(hosts: List<String>, tick: (Int) -> Unit): String? {
        val jobs = hosts.map { h -> async { probe(h).also { tick(1) } } }
        for ((i, job) in jobs.withIndex()) {
            if (job.await()) {
                val rest = jobs.drop(i + 1).filter { it.isActive }
                rest.forEach { it.cancel() }
                tick(rest.size)
                return hosts[i]
            }
        }
        return null
    }

    companion object {
        const val MAX_MX = 2

        /** MX-запрос + два прямых хоста + MX-хосты. */
        const val TOTAL = 1 + 2 + MAX_MX
    }
}
