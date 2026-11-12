# Мост документа ленты (conversation.js → ddmNative): методы зовутся по
# имени из JavaScript, R8 не должен их переименовать или выбросить.
-keepclassmembers class * {
    @android.webkit.JavascriptInterface <methods>;
}
-keepattributes JavascriptInterface

# Angus Mail (MIME и SMTP внешних учёток): провайдеры транспорта, потоки
# кодировок и обработчики типов ищутся по имени (javamail.providers,
# ServiceLoader, mailcap) — R8 не должен их выбросить или переименовать.
-keep class org.eclipse.angus.** { *; }
-keep class jakarta.mail.** { *; }
-keep class jakarta.activation.** { *; }
-dontwarn org.eclipse.angus.**
-dontwarn jakarta.**
-dontwarn javax.security.sasl.**
-dontwarn javax.naming.**
-dontwarn java.awt.**
