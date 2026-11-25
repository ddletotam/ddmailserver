# Сайт lunlun.email

Информационный сайт почтового SaaS **lunlun.email**: лендинг, страница
приложения, инструкции, FAQ. Сервис работает на технологиях ddmail; приложение
**DDMail** — самостоятельный продукт (в магазинах под своим именем), на сайте
оно рекомендуется как клиент. Начат 2026-11-25.

## Сборка

```bash
python3 site/build.py                 # → ~/.cache/builds/lunlun-email-site/
python3 site/build.py --out /tmp/x    # в другой каталог
```

Только стандартная библиотека Python. Сборка падает (exit 1), если есть битая
локальная ссылка, несуществующий якорь или внешний скрипт/стиль/картинка.

Посмотреть локально:

```bash
cd ~/.cache/builds/lunlun-email-site && python3 -m http.server 8765
```

## Устройство

| Путь | Что |
|---|---|
| `config.json` | имя сервиса и приложения, домен, хост почты, адрес поддержки, режим регистрации, ссылки на магазины (`live: false` — кнопка «скоро») |
| `templates/base.html` | шапка, подвал, мета-теги |
| `pages/**/*.html` | страницы; первая строка — `<!--meta {"title","description","nav"} -->` |
| `static/` | стили, логотип (`logo.svg`), единственный скрипт (кнопка меню) |
| `build.py` | сборка + проверка ссылок |

Подстановки в страницах: `{{root}}` — путь до корня (сайт работает и с диска,
и из подкаталога), `{{cfg.ключ.подключ}}` — значение из `config.json`,
`{{snippet:…}}` — кнопки магазинов (`stores-mobile`, `stores-desktop`) и
призыв к регистрации (`signup-href`, `signup-label`, `signup-note`).

Скриншоты и иконки берутся при сборке из `mobile/store-assets/` — те же, что в
магазинах. Политика конфиденциальности — текст `deployments/privacy.html`
(одна редакция на сайт и приложение).

**Внешних ресурсов нет** (шрифты системные, CDN нет) — правило 2026-11-25.
Cookie, аналитики и счётчиков нет.

Стиль — семейство lunlun: тёмно-синий `#282E6C` и молочный `#F9F8F4` из
`lunlun/design/logo-icon.svg`; логотип — плитка lunlun с маленьким конвертом.
Тёмная тема — по настройке системы.

## Выкладка (когда будет домен)

1. DNS: `lunlun.email` A → сервер сайта; `mail.lunlun.email` A → почтовый сервер
   (сейчас 178.215.144.47), плюс MX/SPF/DKIM/DMARC для самого `lunlun.email`.
2. Сертификат на `lunlun.email` и `mail.lunlun.email` (и `www`, если нужен).
3. nginx для сайта:

```nginx
server {
    server_name lunlun.email;
    root /var/www/lunlun.email;
    index index.html;
    error_page 404 /404.html;
    location / { try_files $uri $uri/ =404; }
    # Автонастройка клиентов ищет сервер и на самом домене — отдаём почтовому.
    location /.well-known/ddmail  { proxy_pass https://mail.lunlun.email; }
    location /.well-known/caldav  { return 301 https://mail.lunlun.email/caldav/; }
    location /.well-known/carddav { return 301 https://mail.lunlun.email/carddav/; }
}
```

4. `rsync -a --delete ~/.cache/builds/lunlun-email-site/ сервер:/var/www/lunlun.email/`

## Решить владельцу до запуска

- **Регистрация.** Сейчас `signup.open: false`: кнопка «Оставить заявку» —
  письмо на `support@lunlun.email`. На сервере `disable_registration: true`.
  Откроете — `open: true` и `url` страницы регистрации.
- **Ящик поддержки** `support@lunlun.email` должен существовать.
- **Хост почты** `mail.lunlun.email` — новое имя: в сертификат сервера, в
  `public_endpoints` конфига, в PTR/HELO для доставки.
- **Политика конфиденциальности сервиса.** На сайте — политика приложения
  DDMail. Для SaaS (оператор хранит почту пользователей на своём сервере) нужна
  своя политика и согласие при регистрации — 152-ФЗ; заодно пользовательское
  соглашение.
- **Цены.** В FAQ: «условия сообщаем в ответ на заявку» — заменить, когда будут.
- **Windows и Linux.** Кнопки «скоро» — нет публичной сборки.
- **RuStore.** `live: false`, пока DDMail не опубликован.
- **Скриншоты** — из Android-карточки; в одном видно `review@mail.letotam…`.
