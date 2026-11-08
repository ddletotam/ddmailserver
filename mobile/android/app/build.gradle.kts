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
        versionCode = 4
        versionName = "0.0.4"
    }

    // Один код для Huawei и обычного Android: различается только доставка
    // push (контракт §1, §7).
    flavorDimensions += "push"
    productFlavors {
        create("hms") { dimension = "push" }
        create("gms") { dimension = "push" }
    }

    buildTypes {
        release {
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
    implementation(libs.compose.tooling.preview)
    debugImplementation(libs.compose.tooling)
}
