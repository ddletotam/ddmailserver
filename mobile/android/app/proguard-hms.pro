# Huawei Push Kit (сборка hms): HMS Core находит классы SDK по имени —
# правила из руководства Huawei по интеграции.
-keepattributes *Annotation*
-keepattributes Exceptions
-keepattributes InnerClasses
-keepattributes Signature
-keep class com.hianalytics.android.** { *; }
-keep class com.huawei.updatesdk.** { *; }
-keep class com.huawei.hms.** { *; }
-dontwarn com.huawei.**
-dontwarn com.hianalytics.**
