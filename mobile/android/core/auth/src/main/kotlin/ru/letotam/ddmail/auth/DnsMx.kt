package ru.letotam.ddmail.auth

import android.net.DnsResolver
import android.os.CancellationSignal
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withTimeoutOrNull
import java.util.concurrent.Executors
import kotlin.coroutines.resume

/**
 * MX домена через системный резолвер (`DnsResolver`, API 29+): тот же DNS,
 * что у всего телефона, поэтому работают и внутренние домены (корпоративный
 * VPN, свой DNS). Ошибка или таймаут — пустой список.
 */
object DnsMx {
    private val executor = Executors.newSingleThreadExecutor()

    suspend fun lookup(domain: String, timeoutMs: Long = 5_000): List<String> =
        withTimeoutOrNull(timeoutMs) {
            suspendCancellableCoroutine { cont ->
                val cancel = CancellationSignal()
                cont.invokeOnCancellation { cancel.cancel() }
                DnsResolver.getInstance().rawQuery(
                    null, domain, DnsResolver.CLASS_IN, TYPE_MX, DnsResolver.FLAG_EMPTY,
                    executor, cancel,
                    object : DnsResolver.Callback<ByteArray> {
                        override fun onAnswer(answer: ByteArray, rcode: Int) {
                            if (cont.isActive) cont.resume(parseMxAnswer(answer).map { it.second }.distinct())
                        }

                        override fun onError(error: DnsResolver.DnsException) {
                            if (cont.isActive) cont.resume(emptyList())
                        }
                    },
                )
            }
        } ?: emptyList()

    private const val TYPE_MX = 15
}
