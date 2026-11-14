package ru.letotam.ddmail.push

import android.app.Application

/**
 * Служба push своей сборки (контракт §1, §7): `hms` — Huawei Push Kit,
 * `rustore` — RuStore Push. Реализация — `FlavorPush` в каталоге flavor'а;
 * всё остальное (регистрация на сервере, синк по push, уведомления) общее.
 */
interface PushProvider {
    /** Служба — `kind` регистрации на сервере. */
    val kind: String

    /**
     * Сборка знает свои ключи (App ID Huawei, project id RuStore). Нет —
     * push в ней выключен, новая почта приходит фоновым синком, как до push.
     */
    val configured: Boolean

    /** Подготовка SDK — из `Application.onCreate`. */
    fun init(app: Application)

    /**
     * Попросить у службы токен. Ответ — [PushRegistrar.onNewToken]: сразу или
     * позже, из службы сообщений flavor'а.
     */
    fun requestToken(app: Application)
}
