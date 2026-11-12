// Внешние почтовые учётки (docs/mobile-external-accounts.md): IMAP-клиент,
// разбор и сборка MIME, SMTP, автонастройка, склейка бесед, CalDAV/CardDAV,
// Google OAuth. Чистый Kotlin без Android — тестируется на JVM, как :core:api.
plugins {
    alias(libs.plugins.kotlin.jvm)
    alias(libs.plugins.kotlin.serialization)
}

kotlin {
    jvmToolchain(17)
}

dependencies {
    api(project(":core:api"))
    // MIME и SMTP — Angus Mail (Jakarta Mail). IMAP — свой клиент
    // (ImapConnection): только UID-команды, CONDSTORE/QRESYNC, IDLE.
    api(libs.angus.smtp)
    runtimeOnly(libs.angus.activation)
    implementation(libs.kotlinx.serialization.json)
    testImplementation(libs.junit)
    testImplementation(libs.okhttp.mockwebserver)
    testImplementation(libs.kotlinx.coroutines.test)
}
