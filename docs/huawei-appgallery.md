# Huawei AppGallery: Push Kit, карточка, анкеты, чек-лист владельца

Что сделать в AppGallery Connect (AGC), чтобы у Android-клиента ddmail заработал HMS Push
Kit и его можно было выложить в AppGallery. Тексты карточки — на вычитку владельцу.
Политика конфиденциальности — `deployments/privacy.html`
(https://mail.letotam.ru/privacy, редакция 14.11.2026). Анкета о данных ниже составлена
строго по ней: поменялась политика — анкету правим в том же коммите.

Требования сверены 2026-11-14 по справке AppGallery Connect (ссылки внизу).
Интерфейс консоли меняется — если пункт называется иначе, ищите по смыслу.

## 0. Что нужно и когда

| Цель | Что нужно |
|---|---|
| Пользоваться самому | ничего: APK ставится по USB (`docs/android-behavior-contract.md` §1) |
| Push вместо проверки раз в 15 минут | шаги 1–5. Публиковать приложение не нужно |
| Выложить в AppGallery | шаги 1–8 |

## Сборка

- В AppGallery идёт вариант **`hms`** (HMS Push Kit):
  `./gradlew assembleHmsRelease` →
  `~/.cache/builds/gradle/ddmail-android/app/outputs/apk/hms/release/app-hms-release.apk`.
- Пакет `ru.letotam.ddmail`. Подпись — постоянный ключ `~/.config/ddmail-android/release.jks`,
  SHA-256 сертификата:
  `53:07:94:04:20:DE:9F:B5:F7:04:44:C4:E1:58:33:94:BB:89:98:28:F8:17:47:E4:92:CF:72:38:FE:0B:17:F5`.
- Без `~/.config/ddmail-android/agconnect-services.json` сборка собирается, но push
  выключен: в «Настройках» написано «Без push-сервиса: … примерно раз в 15 минут»
  (так на Mate XT с 0.0.41 2026-11-14). Файла пока нет.

## 1. HUAWEI ID

Подойдёт тот, под которым вы вошли на Mate XT, или новый на
[id.huawei.com](https://id.huawei.com). **Страну/регион аккаунта потом не поменять** —
указывайте Россию.

## 2. Верификация разработчика

1. [developer.huawei.com](https://developer.huawei.com/consumer/en/) → **Console** →
   **Identity verification** → **Individual developer**. Для публикации от ИП или
   организации сразу выбирайте Enterprise: тип потом не меняется.
2. Документы: удостоверение личности и банковский документ. Проверка — 1–2 рабочих дня.

> Документы — ваши персональные данные (152-ФЗ). Передаёте их Huawei сами, через их
> форму; мне их присылать не нужно.

## 3. Проект и приложение в AppGallery Connect

1. **My projects → Add project**, имя `ddmail`.
2. В проекте **Add app**: Platform **Android**, Device **Mobile phone** (и Tablet, если
   предложит), App name `ddmail`, **Package name `ru.letotam.ddmail`** — ровно так,
   Category **App**.
3. **Data storage location** (если спросит) — Россия, если есть в списке, иначе ближайшее
   (Germany). Это место хранения данных push-сервиса у Huawei.

## 4. Отпечаток SHA-256

**Project settings → General information → App information → SHA-256 certificate
fingerprint → Add**:

```
53:07:94:04:20:DE:9F:B5:F7:04:44:C4:E1:58:33:94:BB:89:98:28:F8:17:47:E4:92:CF:72:38:FE:0B:17:F5
```

Подхватывается до ~10 минут. Отладочный отпечаток не нужен: на телефон ставится только
подписанный release.

**App signing (переподпись ключом Huawei) — не включать.** С ней AppGallery выдаёт
пользователям APK с другим ключом: отпечаток выше перестанет совпадать, push сломается, а
обновления из AppGallery и по USB станут несовместимы. Если консоль включит её
принудительно, добавьте в п. 4 и отпечаток ключа Huawei (он показывается на странице App
signing) и скажите мне.

## 5. Push Kit и ключи

1. Проект → **Grow → Push Kit** (или **Engage → Push Kit**) → **Enable now**. Проверьте
   также **Project settings → Manage APIs → Push Kit** — включён.
2. **Project settings → General information → App information → agconnect-services.json**
   — скачать (после того как внесён отпечаток; поменяли отпечатки — скачать заново).
3. Там же — **App ID** и **App secret** (OAuth client secret приложения). Они нужны
   серверу: `push.hms.app_id` / `push.hms.client_secret` в `/etc/mailserver/config.yaml`.

Push несёт только сигнал «есть новые письма», без отправителя, темы и адресов (политика,
раздел «Уведомления»; android-behavior-contract §7).

## 6. Карточка (My apps → ddmail → Version information → Draft)

| Поле (лимит AGC) | Значение |
|---|---|
| Язык | русский (по умолчанию) |
| Название | **DDMail** — во всех сторах и под иконкой (`app_name`), решение владельца 2026-11-14 |
| Краткое описание (до 80, лучше ≤ 25) | Почта, календарь и контакты |
| Полное описание (до 8000) | как в `docs/rustore-listing.md` («Полное описание») |
| Категория (2-й и 3-й уровень) | Apps → **Efficiency** («Эффективность»/офис; почтовые клиенты обычно там). Если подкатегории почты нет, ближайшая к «инструменты/офис». Huawei может поменять категорию сам — придёт письмо |
| Иконка | 216×216 или 512×512 PNG до 2 МБ, квадрат, без анимации. Готовые: `mobile/store-assets/icon-216.png`, `icon-512.png` (их готовит параллельная задача про иконку, сверить, что в git) |
| Privacy policy URL | https://mail.letotam.ru/privacy (открывается из интернета — проверено) |
| Data subject right URL | необязательно, оставить пустым. Обращения — legal@mfml.ru, указаны в политике |
| Страны | Россия (и другие — по решению) |
| Цена | Free. Это настройка консоли, в описании не обещать |
| Тестовый доступ (App review → test account) | `review@mail.letotam.ru` + пароль из `~/.config/ddmail-ios/review-account.env`. Порядок входа и просьба не писать на реальные адреса — как в `docs/rustore-listing.md` («Модерация») |

> Не писать «бесплатно навсегда», «лучший» и подобное: обещаний на будущее политика не
> даёт.

### Скриншоты

Требования AGC для APK: **3–8 штук**, PNG/JPG/JPEG **до 2 МБ** (WebP — до 100 КБ), без
анимации. Вертикальные или горизонтальные, базовое разрешение 450×800 / 800×450. Сторона —
от 320 до 3840 px.

Берём **те же 1080×1920 (9:16) PNG**, что для RuStore: экраны, способ подготовки и
требование демо-учётки — в `docs/rustore-listing.md` («Скриншоты»). Снимок Mate XT
1008×2232 (≈9:20) напрямую не загружать: он не 9:16. В одной версии все скриншоты —
одной ориентации.

Загружать **все 7** из `mobile/store-assets/android/` по порядку (`01-conversations.png` …
`07-group-conversation.png`): все 1080×1920 PNG, каждый меньше 250 КБ — в лимит 2 МБ.

### Возрастной рейтинг (Content rating)

Анкета обязательна. Ответы: насилие, ненормативная лексика, контент для взрослых, азартные
игры, наркотики/алкоголь, покупки — **нет**. Своего контента у приложения нет.
Общение/взаимодействие пользователей — **да**: это почтовый клиент, человек переписывается
с теми, кого выбирает сам; спам и чёрный список доменов есть. Неограниченный доступ в
интернет — **нет**: это не браузер, ссылки открываются в системном браузере. Ожидаемо
**12+**. Если анкета даст ниже — оставить. Выбрать выше можно, ниже — нет.

### Privacy tags (декларация о данных)

Основание — `deployments/privacy.html`. **Collect personal data: Yes.** Сервер ddmail
(mail.letotam.ru) держит оператор, указанный в политике: письма, события, задачи и
контакты учётки ddmail хранятся на нём, а сервер хранит идентификатор устройства для push.

| Сценарий | Тип данных | Откуда (политика) |
|---|---|---|
| App functionality | Email address | адреса учётных записей |
| App functionality | Name / user ID | имена пользователей учётных записей |
| App functionality | Emails / messages | письма: отправители, получатели, темы, тексты, вложения |
| App functionality | Photos (if offered) | картинки во вложениях писем |
| App functionality | Contacts | контакты учётной записи |
| App functionality | Calendar | события календаря, задачи |
| App functionality | Device ID | идентификатор устройства в Huawei Push Kit; хранится до выхода из учётки |

**Advertising and marketing — не выбирать**: рекламы, аналитики и трекеров нет. Данные
третьим лицам не передаются. Push-сервис получает только сигнал «есть новые письма».
Соединения шифруются. Пароли ящиков IMAP/SMTP и CalDAV/CardDAV остаются на устройстве
(Android Keystore) и уходят только своим серверам. Пароль ddmail на устройстве не
хранится, только токен. Удаление учётной записи в приложении удаляет с устройства её
пароль или токен и кэш.

## 7. Пакет и проверка

- **Пакет:** `app-hms-release.apk`, подписанный ключом выше. Собираю я — после того как
  `agconnect-services.json` лежит в `~/.config/ddmail-android/` (тогда в APK есть App ID и
  push включён).
- **Submit for review** — обычно несколько рабочих дней. Отказ приходит с причиной на
  почту аккаунта.
- Можно начать с **тестового выпуска** (Version → Test, open/closed testing по HUAWEI ID):
  он тоже проходит проверку, но виден только тестировщикам.

## 8. Чек-лист владельца (сводно)

В AppGallery Connect:

- [ ] Верификация разработчика пройдена (шаг 2).
- [ ] Проект `ddmail`, приложение Android `ru.letotam.ddmail` (шаг 3).
- [ ] SHA-256 release внесён (шаг 4); App signing **выключен**.
- [ ] Push Kit включён (шаг 5).
- [ ] Скачан `agconnect-services.json` (после отпечатка).
- [ ] Для публикации: карточка, privacy tags, рейтинг, тестовая учётка (шаг 6) —
      скриншоты — `mobile/store-assets/android/` (7 шт.).

**Что прислать мне:**

| Что | Как | Секрет? |
|---|---|---|
| `agconnect-services.json` | файлом в чат или положить в `~/.config/ddmail-android/` на станции. Разложу на обе станции и пересоберу `hms` | нет (он и так внутри каждого APK) |
| App ID | в чат | нет |
| App secret (client secret) | **не в чат.** Положите в файл `~/.config/ddmail-android/hms-app-secret` на станции (вне git), оттуда я перенесу его в `push.hms.client_secret` конфига сервера | **да** |
| Решения: название (ddmail/DDMail), публикация (только push / тест / публично) | в чат | нет |

Чего **не** присылать: документы верификации, пароль HUAWEI ID.

## Источники

- [AGC: публикация APK — описание, категория, рейтинг, privacy tags](https://developer.huawei.com/consumer/en/doc/app/agc-help-releaseapkrpk-0000001106463276)
- [AGC: требования к иконке и скриншотам](https://developer.huawei.com/consumer/en/doc/app/agc-help-app-material-requirement-0000001146534651)
- [HUAWEI Developers: Identity Verification](https://developer.huawei.com/consumer/en/doc/help/identityverfication-0000001053292680)
- [AppGallery Connect: Getting Started with Android](https://developer.huawei.com/consumer/en/doc/AppGallery-connect-Guides/agc-get-started-android-0000001058210705)
- [Push Kit: Configuring App Information in AppGallery Connect](https://developer.huawei.com/consumer/en/doc/HMS-Plugin-Guides-V1/config-agc-0000001050178043-V1)
