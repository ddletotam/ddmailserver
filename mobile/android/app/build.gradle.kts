import java.util.Properties

plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.android)
    alias(libs.plugins.kotlin.compose)
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
        versionCode = 29
        versionName = "0.0.29"
    }

    // Один код для Huawei и обычного Android: различается только доставка
    // push (контракт §1, §7).
    flavorDimensions += "push"
    productFlavors {
        create("hms") { dimension = "push" }
        create("gms") { dimension = "push" }
    }

    // Ключ подписи — вне git, в каталоге секретов станции (контракт §7):
    // ~/.config/ddmail-android/ или %APPDATA%\ddmail-android\. Нет его —
    // release собирается неподписанным (на телефон такой не поставить).
    val secrets = System.getenv("APPDATA")?.let { File(it, "ddmail-android") }
        ?: File(System.getProperty("user.home"), ".config/ddmail-android")
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

dependencies {
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
