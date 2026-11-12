package ru.letotam.ddmail.imap

import java.io.IOException
import java.net.InetSocketAddress
import java.net.Socket
import java.net.SocketTimeoutException
import java.net.UnknownHostException
import javax.net.ssl.SSLException
import javax.net.ssl.SSLSocket
import javax.net.ssl.SSLSocketFactory

/**
 * TLS для почтовых протоколов (контракт §10): хранилище доверия системы
 * (на Android — с пользовательскими CA, через network_security_config),
 * только TLS 1.2+, имя хоста проверяется всегда. Исключений и «доверять всё
 * равно» нет.
 */
object Tls {
    private val factory: SSLSocketFactory get() = SSLSocketFactory.getDefault() as SSLSocketFactory

    /** TCP до хоста с таймаутом установки. */
    fun tcp(host: String, port: Int, connectTimeoutMs: Int): Socket {
        val s = Socket()
        try {
            s.connect(InetSocketAddress(host, port), connectTimeoutMs)
        } catch (e: IOException) {
            runCatching { s.close() }
            throw networkError(e)
        }
        return s
    }

    /** Поверх открытого сокета (неявный TLS или после STARTTLS). */
    fun wrap(plain: Socket, host: String, port: Int): SSLSocket {
        try {
            val s = factory.createSocket(plain, host, port, true) as SSLSocket
            s.enabledProtocols = s.supportedProtocols.filter { it == "TLSv1.2" || it == "TLSv1.3" }.toTypedArray()
            s.startHandshake()
            if (!okhttp3.internal.tls.OkHostnameVerifier.verify(host, s.session)) {
                runCatching { s.close() }
                throw MailException.Network("TLS: сертификат выдан не для $host")
            }
            return s
        } catch (e: SSLException) {
            runCatching { plain.close() }
            throw MailException.Network("TLS: ${e.message ?: e.javaClass.simpleName}", e)
        } catch (e: IOException) {
            runCatching { plain.close() }
            throw networkError(e)
        }
    }

    fun networkError(e: IOException): MailException.Network = when (e) {
        is SocketTimeoutException -> MailException.Network("Сервер не отвечает", e)
        is UnknownHostException -> MailException.Network("Сервер не найден: ${e.message}", e)
        is SSLException -> MailException.Network("TLS: ${e.message}", e)
        else -> MailException.Network(e.message ?: "Нет соединения", e)
    }
}
