// Android-клиент ddmail. Устройство и поведение — docs/android-behavior-contract.md.
pluginManagement {
    repositories {
        google()
        mavenCentral()
        gradlePluginPortal()
    }
}
dependencyResolutionManagement {
    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
    repositories {
        google()
        mavenCentral()
        // Push сборок (контракт §7): Huawei Push Kit — сборка hms, RuStore
        // Push — сборка rustore. Каждый репозиторий отдаёт только свои группы.
        maven("https://developer.huawei.com/repo/") {
            content { includeGroupByRegex("com\\.huawei\\..*") }
        }
        maven("https://nexus-external.rustore.ru/repository/maven-rustore-exposed/") {
            content { includeGroupByRegex("ru\\.rustore\\..*") }
        }
    }
}

rootProject.name = "ddmail-android"
include(":app", ":core:api", ":core:auth", ":core:db", ":core:mail", ":core:sync")
