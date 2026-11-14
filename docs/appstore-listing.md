# App Store: карточка, анкеты, заметки для ревью

Черновик для публикации Apple-клиента (`mobile/ios`, bundle `ru.letotam.ddmail`,
команда `F2MHZQSN5H`). Тексты на вычитку владельцу. Политика конфиденциальности —
`deployments/privacy.html` (https://mail.letotam.ru/privacy); анкета App Privacy ниже
составлена строго по ней. Если поменяется политика, анкету нужно поправить в том же коммите.

## Запись приложения в App Store Connect

App Store Connect API не создаёт приложения: `POST /v1/apps` отвечает
`403 … does not allow 'CREATE'` (проверено 2026-11-14). Создаёт владелец:
**Apps → «+» → New App**:

| Поле | Значение |
|---|---|
| Platforms | iOS **и** macOS (одна запись, Universal Purchase: bundle id `ru.letotam.ddmail` — UNIVERSAL) |
| Name | DDMail |
| Primary Language | Russian |
| Bundle ID | ru.letotam.ddmail (XC ru letotam ddmail) |
| SKU | ddmail-ios |
| User Access | Full Access |

Потом сборка загружается одной командой: `mobile/ios/scripts/mac.sh archive ios upload`
(и `archive mac upload` для macOS), см. контракт iOS §1.

## Карточка (ru)

**Название (до 30):** DDMail

**Подзаголовок (до 30):** Почта, календарь и контакты

**Категория:** основная — «Производительность» (Productivity; так же в
`LSApplicationCategoryType`), дополнительная — «Бизнес».

**Ключевые слова (до 100, через запятую без пробелов):**
`почта,email,imap,smtp,календарь,контакты,задачи,беседы,переписка,caldav,carddav,ящик,ddmail`

**Рекламный текст (до 170, можно менять без ревью):**
Переписка с человеком — одна лента, как в мессенджере. Почта, календарь, задачи и контакты в одном приложении.

**Описание:**

DDMail — почтовый клиент для сервера ddmail и любых ящиков IMAP/SMTP. Письма собраны в беседы: переписка с человеком или группой — одна лента, как в мессенджере, а не плоский список писем.

• Беседы вместо писем: ответы, вложения и история переписки — в одной ленте.
• Папки диалогов с правилами по адресу получателя и домену собеседника — те же, что в настольной версии.
• Цвет беседы по адресу, на который пришло письмо: рабочее и личное видно сразу.
• Несколько учётных записей: сервер ddmail и обычные ящики IMAP/SMTP с автонастройкой.
• Картинки из писем не загружаются без вашего разрешения.
• Спам — в отдельной скрытой папке, без уведомлений.
• Календарь с неделей, месяцем и годом, напоминания о встречах, задачи и контакты; внешние календари и адресные книги по CalDAV/CardDAV.
• iPhone, iPad и Mac: на большом экране список и беседа рядом.

Для учётной записи ddmail нужен сервер ddmail. Пароль учётной записи ddmail на устройстве не хранится — только токен сессии в Связке ключей. Приложение не содержит рекламы, аналитики и трекеров.

**Что нового (для 0.0.14):** Первая версия для App Store.

**URL поддержки:** https://mail.letotam.ru/privacy (в разделе «Оператор персональных
данных» есть адрес обращений legal@mfml.ru). Отдельной страницы поддержки пока нет. Если
ревью придерётся, нужна страница с контактом, например `https://mail.letotam.ru/support`.
**URL маркетинга:** не обязателен, можно оставить пустым.
**URL политики конфиденциальности:** https://mail.letotam.ru/privacy
**Copyright:** 2026 Денис Данилин

**Скриншоты:** iPhone 6.9″ (1320×2868, симулятор iPhone 18 Pro Max), iPad 13″
(2064×2752, iPad Pro 13-inch (M5)), Mac (2880×1800). Снимает UI-тест
`AppStoreShotsTests` под демо-учёткой (список с табами папок, беседа, «Написать по
адресу», календарь, контакты, задачи):
`DDMAIL_TEST_EMAIL=review@mail.letotam.ru DDMAIL_TEST_PASSWORD=… mac.sh uitest iphonemax AppStoreShotsTests`
(и `ipad`), перед этим на симуляторах `xcrun simctl status_bar <udid> override --time 9:41
--batteryLevel 100 --batteryState charged --cellularBars 4 --wifiBars 3`. В git снимков нет.
Mac-снимки по SSH снять нельзя (нет разрешения «Запись экрана»), поэтому macOS-версия
на первое ревью не подаётся. Снимать на демо-учётке ниже: в ней
только вымышленные люди и адреса `example.com`/`example.org`. Скриншоты с настоящей почты
не годятся: на них персональные данные корреспондентов (152-ФЗ).

## Возрастной рейтинг

Анкета (Age Ratings): на все вопросы о контенте (насилие, ненормативная лексика,
азартные игры, контент для взрослых и т. п.) — **None / Нет**: своего контента в
приложении нет, только почта пользователя.

- Unrestricted Web Access — **No**. Это не браузер: HTML писем показывается в ленте,
  внешние ссылки открываются в системном браузере.
- Messaging and Chat / User-Generated Content — **Yes**. Это почтовый клиент: человек
  переписывается с людьми, которых выбирает сам. Пожаловаться на сообщение и
  заблокировать отправителя можно через «В спам» и чёрный список домена.
- Advertising, In-App Purchases — **No**.

Решение владельца 2026-11-14 — **12+** во всех сторах. Анкета даёт 4+. В новой шкале
Apple (с 2025: 4+/9+/13+/16+/18+) 12+ нет, поэтому стоит ближайшее не ниже:
`ageRatingOverrideV2 = THIRTEEN_PLUS`. ASC при этом показывает `appStoreAgeRating =
TWELVE_PLUS` (старая шкала), Бразилия — 12.

## App Privacy (анкета «Конфиденциальность приложения»)

Основание — `deployments/privacy.html` (редакция 14.11.2026). Рекламы, аналитики и
трекеров нет, отслеживания (Tracking) нет.

**Собирает ли приложение данные?** — **Да.** Сервер ddmail (mail.letotam.ru) держит
оператор, указанный в политике: письма, события, задачи и контакты учётки ddmail хранятся
на нём, а сервер хранит push-токен устройства. По определению Apple данные, хранящиеся на
сервере разработчика дольше, чем нужно для обработки запроса, считаются собранными.

Чего в анкете нет: данных ящиков IMAP/SMTP и CalDAV/CardDAV. Приложение ходит к серверам,
которые человек подключил сам, разработчику эти данные не передаются (политика:
«Кому передаются данные»). Пароли тоже не указываем: от ddmail пароль не хранится, только
токен, а пароли внешних ящиков остаются в Связке ключей на устройстве.

| Тип данных (Apple) | Собирается | Связан с личностью | Трекинг | Цель |
|---|---|---|---|---|
| Contact Info → Email Address | Да (адрес учётки ddmail) | Да | Нет | App Functionality |
| Contact Info → Name | Да (имя пользователя учётки) | Да | Нет | App Functionality |
| User Content → Emails or Text Messages | Да (письма на сервере ddmail) | Да | Нет | App Functionality |
| User Content → Photos or Videos | Да (изображения во вложениях писем) | Да | Нет | App Functionality |
| User Content → Other User Content | Да (события календаря, задачи) | Да | Нет | App Functionality |
| Contacts | Да (контакты адресной книги ddmail) | Да | Нет | App Functionality |
| Identifiers → Device ID | Да (токен APNs для push, хранится до выхода из учётки) | Да | Нет | App Functionality |

Всё остальное (Location, Health, Financial, Browsing/Search History, Usage Data,
Diagnostics, Purchases, Sensitive Info) — **не собирается**. Отчётов о сбоях и аналитики
в приложении нет.

## Заметки для App Review (App Review Information)

**Sign-in required:** Yes.
**User name:** `review@mail.letotam.ru`
**Password:** вводится только в App Store Connect, в поле Password раздела Sign-In
Information. В git пароля нет; на станции сборки он лежит в
`~/.config/ddmail-ios/review-account.env`.
**Контакт:** Денис Данилин, legal@mfml.ru, телефон в формате `+7 …`. API без телефона
контакт не сохраняет (`contactPhone` обязателен), поэтому все три поля вводит владелец.

**Notes (англ., для ревьюера):**

> DDMail is an email client for a ddmail mail server and for any IMAP/SMTP mailbox.
> To sign in with the demo account: on the first screen enter the email address
> `review@mail.letotam.ru` and tap "Найти сервер" (Find server). The app discovers the server
> (mail.letotam.ru) automatically and fills in the user name. Then enter the password from the
> Sign-In Information and tap "Войти" (Sign in).
>
> The demo account contains fictitious data only: about 20 conversations (incoming mail, the
> user's own replies, an image attachment, HTML newsletters), three dialog folders
> ("Работа", "Рассылки", "Путешествия") shown as tabs above the list, calendar events for this
> and next week, tasks and contacts. They are reachable from the menu (☰): Почта / Календарь /
> Контакты / Задачи / Настройки. New mail is started from the search field: type an address and
> pick "Написать по адресу…".
>
> Please do not send mail to real external addresses from the demo account: the example.com /
> example.org correspondents do not exist and such mail will bounce.
>
> Encryption: the app uses only the encryption built into the OS (HTTPS/TLS via URLSession and
> Network.framework); ITSAppUsesNonExemptEncryption = NO.

## Состояние в App Store Connect (2026-11-14)

Заполнено через ASC API (app id 6834889461), iOS-версия `0.0.14` (`db6bb49c-…`; создавалась
как «1.0», переименована):

- app: `contentRightsDeclaration = DOES_NOT_USE_THIRD_PARTY_CONTENT`;
- appInfo: категории PRODUCTIVITY / BUSINESS; ru: имя, подзаголовок, политика URL;
- версия: copyright, сборка 14 (iOS) привязана; ru: описание, ключевые слова,
  рекламный текст, support URL. «Что нового» у первой версии не заполняется;
- возрастная анкета: всё «нет», messagingAndChat и userGeneratedContent — «да»,
  повышение до 13+ (см. выше);
- цена: бесплатно (база USA), доступность: все 175 территорий плюс новые;
- App Review: демо-учётка, пароль, заметки (текст выше);
- скриншоты: 6 штук iPhone 6.9″ (`APP_IPHONE_67`) и 5 штук iPad 13″
  (`APP_IPAD_PRO_3GEN_129`); у iPad нет отдельного экрана списка, на снимке беседы
  список и беседа стоят рядом.

Проверка `reviewSubmissionItems` (черновик reviewSubmission для iOS уже создан) называет
только два недостающих пункта:
1. App Review → Contact Information: имя, фамилия, email, телефон;
2. App Privacy: ответы по таблице выше, затем Publish. В публичном API этого раздела нет.

macOS-версия (`1.0`, PREPARE_FOR_SUBMISSION) не заполнялась. Без настоящих Mac-скриншотов
её не подаём.

## Демо-учётка (сервер)

Заведена 2026-11-14 штатным путём сервера: `POST /api/register` создаёт пользователя
`review` (id 26), от его сессии `POST /api/domains` добавляет домен `mail.letotam.ru` и
`POST /api/mailboxes` ящик `review@mail.letotam.ru`. Календари «Работа», «Личное» и
«Задачи» созданы через `/calendars/create` и включены через `/calendars/{id}/enabled`,
адресная книга «Контакты» — через `/contacts/addressbooks/create`. Письма импортированы
IMAP APPEND (входящие в INBOX, свои ответы в Sent). События, задачи, контакты и папки
диалогов созданы через `/api/desktop/v1` этой же учётки.

- Почему не `review@letotam.ru`: MX `letotam.ru` — iCloud. Если сделать `letotam.ru`
  локальным доменом сервера, сервер начнёт доставлять у себя или отклонять письма своих
  пользователей на настоящие адреса `@letotam.ru`. `mail.letotam.ru` почту не принимал и
  автонастройкой находится сразу (`/.well-known/ddmail`).
- Две правки SQL: штатного пути не было, а без них задачи и контакты недоступны.
  1. `calendars.supported_components='VTODO'` у «Задачи». Локальный календарь
     создаётся только для событий.
  2. `address_books.enabled=true` у «Контакты». Локальная книга создаётся
     выключенной, а переключателя у адресных книг нет. Календари создаются выключенными
     так же, но их можно включить штатно. См. backlog.
- Данные вымышленные: корреспонденты на `corp.example.com`, `news.example.org`,
  `shop.example.org`, `travel.example.com`, `example.org`, `example.com`.
