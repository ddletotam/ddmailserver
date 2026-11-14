import java.util.Properties

plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.android)
    alias(libs.plugins.kotlin.compose)
}

// Секреты и ключи сборки — вне git, в каталоге станции (контракт §1, §7):
// ~/.config/ddmail-android/ или %APPDATA%\ddmail-android\.
val secrets = System.getenv("APPDATA")?.let { File(it, "ddmail-android") }
    ?: File(System.getProperty("user.home"), ".config/ddmail-android")

// Huawei Push Kit (сборка hms): agconnect-services.json из AppGallery Connect.
// Плагин agconnect не нужен: Push Kit читает файл из assets, а App ID берём
// из него сами. Нет файла — сборка собирается без push (фоновый синк).
val agconnectJson = File(secrets, "agconnect-services.json")
val hmsAppId: String = agconnectJson.takeIf { it.isFile }?.let { f ->
    @Suppress("UNCHECKED_CAST")
    val root = groovy.json.JsonSlurper().parse(f) as Map<String, Any?>
    val client = root["client"] as? Map<String, Any?>
    val app = root["app_info"] as? Map<String, Any?>
    (client?.get("app_id") ?: app?.get("app_id"))?.toString()
}.orEmpty()

// RuStore Push (сборка rustore): project id из RuStore Консоли —
// rustore.properties (`project_id=…`) станции. Нет — сборка без push.
val rustoreProjectId: String = File(secrets, "rustore.properties").takeIf { it.isFile }?.let { f ->
    Properties().apply { f.inputStream().use { load(it) } }.getProperty("project_id")
}.orEmpty().trim()

/** agconnect-services.json → assets сборки hms (только он, не весь каталог секретов). */
abstract class AgconnectAssets : DefaultTask() {
    @get:InputFiles
    abstract val source: ConfigurableFileCollection

    @get:OutputDirectory
    abstract val outputDir: DirectoryProperty

    @TaskAction
    fun copy() {
        val out = outputDir.get().asFile
        out.deleteRecursively()
        out.mkdirs()
        source.files.filter { it.isFile }.forEach { it.copyTo(File(out, "agconnect-services.json"), overwrite = true) }
    }
}

val agconnectAssets = tasks.register<AgconnectAssets>("agconnectAssets") {
    source.from(agconnectJson.takeIf { it.isFile } ?: emptyList<File>())
    outputDir.set(layout.buildDirectory.dir("generated/agconnect"))
}

android {
    namespace = "ru.letotam.ddmail"
    compileSdk = 35

    defaultConfig {
        // Тот же идентификатор, что у десктопа; под ним же — проект в
        // AppGallery Connect (HMS Push).
        applicationId = "ru.letotam.ddmail"
        minSdk = 29
        targetSdk = 35
        // Бампается при любой правке клиента (контракт §1).
        versionCode = 38
        versionName = "0.0.38"
    }

    // Один код для Huawei и прочих Android: различается только доставка
    // push (контракт §1, §7). hms — Huawei Push Kit (AppGallery, телефоны
    // Huawei); rustore — RuStore Push (RuStore, остальные телефоны).
    flavorDimensions += "push"
    productFlavors {
        create("hms") {
            dimension = "push"
            buildConfigField("String", "HMS_APP_ID", "\"$hmsAppId\"")
            manifestPlaceholders["hmsAppId"] = hmsAppId
            proguardFile("proguard-hms.pro")
        }
        create("rustore") {
            dimension = "push"
            buildConfigField("String", "RUSTORE_PROJECT_ID", "\"$rustoreProjectId\"")
        }
    }

    // Ключ подписи — вне git, в каталоге секретов станции. Нет его —
    // release собирается неподписанным (на телефон такой не поставить).
    val keyProps = File(secrets, "keystore.properties")
    signingConfigs {
        if (keyProps.exists()) {
            val p = Properties().apply { keyProps.inputStream().use { load(it) } }
            create("release") {
                storeFile = File(secrets, p.getProperty("storeFile"))
                storePassword = p.getProperty("storePassword")
                keyAlias = p.getProperty("keyAlias")
                keyPassword = p.getProperty("keyPassword")
            }
        }
    }

    buildTypes {
        release {
            signingConfigs.findByName("release")?.let { signingConfig = it }
            isMinifyEnabled = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
    // Документ ленты беседы — общий с будущим iOS-клиентом (контракт §8).
    sourceSets["main"].assets.srcDir("../../shared/conversation-doc")

    // Angus Mail: лицензии в каждом jar — одна на APK; описания провайдеров и
    // типов (javamail.*, mailcap) сливаются, а не выбираются.
    packaging {
        resources {
            excludes += setOf("META-INF/NOTICE.md", "META-INF/LICENSE.md")
            merges += setOf("META-INF/javamail.*", "META-INF/mailcap*", "META-INF/mimetypes.default")
        }
    }

    buildFeatures {
        compose = true
        buildConfig = true
    }
}

androidComponents {
    onVariants(selector().withFlavor("push" to "hms")) { v ->
        v.sources.assets?.addGeneratedSourceDirectory(agconnectAssets, AgconnectAssets::outputDir)
    }
}

dependencies {
    "hmsImplementation"(libs.hms.push)
    "rustoreImplementation"(libs.rustore.push)
    implementation(project(":core:auth"))
    implementation(project(":core:sync"))
    implementation(libs.androidx.lifecycle.runtime.compose)
    implementation(libs.androidx.webkit)
    implementation(libs.androidx.work)
    implementation(libs.kotlinx.serialization.json)
    implementation(libs.androidx.datastore.preferences)
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(libs.kotlinx.coroutines.android)
    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.activity.compose)
    implementation(libs.androidx.lifecycle.runtime)
    implementation(platform(libs.compose.bom))
    implementation(libs.compose.ui)
    implementation(libs.compose.material3)
    implementation(libs.compose.icons)
    implementation(libs.compose.tooling.preview)
    debugImplementation(libs.compose.tooling)
    testImplementation(libs.junit)
}
