# Мост документа ленты (conversation.js → ddmNative): методы зовутся по
# имени из JavaScript, R8 не должен их переименовать или выбросить.
-keepclassmembers class * {
    @android.webkit.JavascriptInterface <methods>;
}
-keepattributes JavascriptInterface
