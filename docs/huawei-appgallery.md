# Huawei: аккаунт разработчика, Push Kit и AppGallery

Инструкция для владельца проекта: что сделать в кабинете Huawei, чтобы у
Android-клиента ddmail заработал HMS Push Kit и чтобы его можно было
выложить в AppGallery. Шаги сверены с документацией HUAWEI Developers на
2026-11-08; интерфейс консоли Huawei меняется — если пункт меню называется
иначе, ищите по смыслу.

## 0. Что из этого нужно и когда

| Цель | Что нужно |
|---|---|
| Пользоваться клиентом самому | ничего: APK ставится по USB (`docs/android-behavior-contract.md` §1) |
| Push-уведомления (вместо опроса раз в 15 минут) | шаги 1–5: аккаунт, проект, Push Kit. **Публиковать приложение не нужно** |
| Выложить в AppGallery для других | шаги 1–7 |

## 1. HUAWEI ID

Подойдёт тот же HUAWEI ID, под которым вы вошли на Mate XT. Если хотите
отделить разработку от личного — заведите новый на
[id.huawei.com](https://id.huawei.com) (email или телефон).

**Страну/регион аккаунта потом поменять нельзя** — указывайте Россию.

## 2. Верификация разработчика

1. Войдите на [developer.huawei.com](https://developer.huawei.com/consumer/en/)
   → справа вверху **Console** (или **Identity verification**).
2. Выберите **Individual developer**.
   - Сменить тип на Enterprise потом нельзя — только удалить аккаунт и
     завести новый. Если планируете публиковаться от ИП/организации,
     сразу выбирайте Enterprise.
3. Заполните поля и загрузите документы: удостоверение личности
   (паспорт или иной документ с фото) **и банковский документ**.
   - Требования: всё читается, фото чёткое, документ не изменён.
4. Проверка — 1–2 рабочих дня. Пока заявка на проверке, данные не
   правятся: только отменить (Developer center → Settings → My profile)
   и подать заново.

Регистрация бесплатная. Какой именно банковский документ Huawei примет у
резидента России сейчас (карта, выписка), в документации не сказано —
если вариант с картой не пройдёт, попробуйте выписку из банка с вашими
ФИО и реквизитами.

> Документы — ваши персональные данные (152-ФЗ). Передаёте их Huawei
> сами, через их форму; мне их присылать не нужно.

## 3. Проект и приложение в AppGallery Connect

1. [AppGallery Connect](https://developer.huawei.com/consumer/en/service/josp/agc/index.html)
   → **My projects** → **Add project**, имя — `ddmail`.
2. В проекте → **Add app**:
   - Platform: **Android**;
   - Device: **Mobile phone** (и Tablet, если предложит);
   - App name: `ddmail`;
   - **Package name: `ru.letotam.ddmail`** — ровно так, это
     `applicationId` клиента;
   - Category: App → Business / Tools.
3. Если консоль спросит **Data storage location** — выберите
   расположение в России, если оно есть в списке; иначе — ближайшее
   (Germany). От этого зависит, где Huawei хранит данные push-сервиса.

## 4. Ключ подписи и отпечаток SHA-256

Push Kit доверяет только приложению, подписанному известным ему ключом.

- **Ключ сделаю я** (release keystore) и положу вне git, в
  `~/.config/ddmail-android/` (контракт §7). Вам пришлю **SHA-256
  отпечаток** — строку вида `AB:CD:…`.
- Вы вносите его: проект → **Project settings → General information →
  App information → SHA-256 certificate fingerprint → Add**.
- Отпечаток подхватывается до ~10 минут.
- Для отладочной сборки нужен свой отпечаток — пришлю оба, внесите оба.

## 5. Push Kit и конфигурационный файл

1. Проект → **Grow → Push Kit** (в новых версиях консоли: **Build /
   Engage → Push Kit** или **Develop → APIs enabled**) → **Enable now**.
2. **Project settings → General information → App information →
   agconnect-services.json** — скачать.
   - Файл нужно перекачивать после каждого изменения отпечатков.
3. Положить его в `~/.config/ddmail-android/agconnect-services.json` на
   станции сборки (в git не кладётся; Windows —
   `%APPDATA%\ddmail-android\`). Можно просто прислать мне файл — сам
   разложу.
4. Для отправки push сервером ddmail нужны **App ID** и
   **Client secret** (OAuth 2.0 client ID/secret приложения) — они на той
   же странице App information. Это секрет: передайте его не в чат, а
   положите сами на сервер (`/etc/mailserver/config.yaml`, ключ скажу
   при реализации) или в файл в `~/.config/ddmail-android/`.

После этого я делаю серверную часть (регистрация токенов устройств,
отправка через HMS Push REST API) и клиентскую (`:push:hms`). Push
несёт только факт «есть новое» — ни отправителя, ни темы (контракт §7,
152-ФЗ).

## 6. Что нужно для публикации (если решите выкладывать)

Карточка приложения: **My apps → ddmail → Version information → Draft**.

- **Описание** (краткое и полное), **категория**, **язык** — русский.
- **Иконка** 216×216 PNG — подготовлю из иконки приложения.
- **Скриншоты** телефона (3–5 штук; Mate XT в сложенном и разложенном
  виде — сниму сам).
- **Политика конфиденциальности — обязательна**, ссылкой. Предлагаю
  страницу на `https://mail.letotam.ru/privacy` — текст подготовлю.
- **Возрастной рейтинг** — анкета в консоли (почта: 3+/12+).
- **Страны распространения** — минимум Россия.
- **Пакет**: release APK или AAB, подписанный ключом из шага 4 (соберу).
  Huawei может предложить **App signing** (переподписывать своим ключом) —
  можно отказаться, тогда подпись остаётся нашей.

## 7. Проверка и выпуск

- **Submit for review** — проверка Huawei, обычно несколько рабочих дней;
  отказ приходит с причиной на почту аккаунта.
- Вместо публичного выпуска можно сначала **тестовый**: Version → **Test**
  (open/closed testing) — список тестировщиков по их HUAWEI ID; тоже идёт
  через проверку, но выпуск виден только им.

## Что прислать мне

1. Подтверждение, что верификация прошла и проект создан.
2. `agconnect-services.json` (после того как внесёте мои отпечатки).
3. Решение по публикации: только push для себя, тест для круга людей или
   публичный выпуск. Для публикации — согласие на страницу политики
   конфиденциальности на `mail.letotam.ru`.

## Источники

- [HUAWEI Developers: Identity Verification](https://developer.huawei.com/consumer/en/doc/help/identityverfication-0000001053292680)
- [HUAWEI ID Registration and Verification](https://developer.huawei.com/consumer/en/doc/start/registration-and-verification-0000001053628148)
- [AppGallery Connect: Getting Started with Android](https://developer.huawei.com/consumer/en/doc/AppGallery-connect-Guides/agc-get-started-android-0000001058210705)
- [Push Kit: Configuring App Information in AppGallery Connect](https://developer.huawei.com/consumer/en/doc/HMS-Plugin-Guides-V1/config-agc-0000001050178043-V1)
- [HMS: подготовка к интеграции (codelab)](https://developer.huawei.com/consumer/en/codelab/HMSPreparation/index.html)
- [Phiture: Listing on AppGallery, step-by-step](https://phiture.com/asostack/listing-on-appgallery-a-step-by-step-guide-and-a-glance-of-the-developer-console-4dcc6169a7c0/)
