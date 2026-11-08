plugins {
    alias(libs.plugins.android.application) apply false
    alias(libs.plugins.android.library) apply false
    alias(libs.plugins.kotlin.android) apply false
    alias(libs.plugins.kotlin.compose) apply false
    alias(libs.plugins.kotlin.jvm) apply false
    alias(libs.plugins.kotlin.serialization) apply false
}

// Сборка — вне дерева проекта (правило workspace): ~/.cache/builds на Linux,
// %LOCALAPPDATA%\builds на Windows. Каждый модуль — в свой подкаталог.
val buildsRoot: File = System.getenv("LOCALAPPDATA")
    ?.let { File(it, "builds") }
    ?: File(System.getProperty("user.home"), ".cache/builds")
val projectBuilds = File(buildsRoot, "gradle/ddmail-android")

allprojects {
    val sub = if (path == ":") "root" else path.trimStart(':').replace(':', '/')
    layout.buildDirectory.set(File(projectBuilds, sub))
}
