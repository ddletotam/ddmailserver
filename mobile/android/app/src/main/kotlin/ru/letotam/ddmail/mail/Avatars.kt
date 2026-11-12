package ru.letotam.ddmail.mail

import android.content.Context
import android.graphics.Bitmap
import android.graphics.ImageDecoder
import android.util.LruCache
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Semaphore
import kotlinx.coroutines.sync.withPermit
import kotlinx.coroutines.withContext
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.sync.MailHub
import java.io.File
import java.nio.ByteBuffer
import java.security.MessageDigest

/**
 * Аватарки собеседников с сервера (`/avatars?email=`, та же цепочка, что у
 * десктопа: свои контакты → Gravatar → BIMI → favicon). Кэш — в памяти и
 * на диске; «аватара нет» тоже запоминается (повтор через неделю), чтобы
 * список не спрашивал сервер на каждую прокрутку. Не больше 4 запросов разом.
 *
 * SVG (BIMI-логотипы) не рисуем: Android их не декодирует — будут инициалы.
 */
class Avatars(context: Context, private val hub: MailHub) {
    private val dir = File(context.cacheDir, "avatars").apply { mkdirs() }
    private val memory = LruCache<String, Holder>(300)
    private val gate = Semaphore(4)

    private class Holder(val image: ImageBitmap?)

    fun cached(email: String): ImageBitmap? = memory.get(key(email))?.image

    suspend fun get(email: String): ImageBitmap? {
        if (email.isBlank()) return null
        val k = key(email)
        memory.get(k)?.let { return it.image }
        val img = withContext(Dispatchers.IO) { fromDisk(k) ?: fromServer(email, k) }
        memory.put(k, Holder(img))
        return img
    }

    private fun fromDisk(k: String): ImageBitmap? {
        val f = File(dir, "$k.img")
        if (f.exists()) return decode(f.readBytes())
        return null
    }

    private suspend fun fromServer(email: String, k: String): ImageBitmap? {
        val none = File(dir, "$k.none")
        if (none.exists() && System.currentTimeMillis() - none.lastModified() < WEEK) return null
        val bin = gate.withPermit {
            try {
                hub.avatar(email)
            } catch (e: ApiException) {
                return null
            }
        }
        if (bin == null) {
            none.writeText("")
            return null
        }
        val img = decode(bin.bytes) ?: run {
            none.writeText("")
            return null
        }
        File(dir, "$k.img").writeBytes(bin.bytes)
        return img
    }

    private fun decode(bytes: ByteArray): ImageBitmap? = try {
        val src = ImageDecoder.createSource(ByteBuffer.wrap(bytes))
        ImageDecoder.decodeBitmap(src) { decoder, info, _ ->
            decoder.allocator = ImageDecoder.ALLOCATOR_SOFTWARE
            val max = maxOf(info.size.width, info.size.height)
            if (max > SIZE) decoder.setTargetSampleSize(max / SIZE)
        }.copy(Bitmap.Config.ARGB_8888, false).asImageBitmap()
    } catch (e: Exception) {
        null
    }

    private fun key(email: String): String =
        MessageDigest.getInstance("SHA-1").digest(email.trim().lowercase().toByteArray())
            .joinToString("") { "%02x".format(it) }

    private companion object {
        const val SIZE = 128
        const val WEEK = 7L * 24 * 3600 * 1000
    }
}
