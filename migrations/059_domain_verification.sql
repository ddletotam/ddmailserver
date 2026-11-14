-- 059: подтверждение владения доменом.
--
-- До этой миграции любой зарегистрированный пользователь мог добавить себе
-- любой домен (хоть gmail.com), и сервер сразу считал его локальным: письма
-- остальных пользователей на этот домен переставали уходить наружу и
-- доставлялись в ящики захватчика (или отбивались 550 «No such user»).
--
-- Теперь домен влияет на маршрутизацию, приём через MX и право отправлять
-- «от» него только после подтверждения: TXT-записью с токеном
-- (verified_method = 'dns_txt') или добавлением администратором ('admin').
-- Пока домен не подтверждён, его могут «заявить» несколько пользователей —
-- уникальность имени держится только среди подтверждённых, поэтому чужая
-- заявка не блокирует настоящего владельца.
--
-- Существующие домены считаются подтверждёнными ('legacy') — это домены,
-- которые сервер уже обслуживает. Исключение — общеизвестные публичные
-- почтовые домены: их владельцем пользователь сервера быть не может, такие
-- строки остаются неподтверждёнными и перестают влиять на доставку.

ALTER TABLE domains ADD COLUMN IF NOT EXISTS verified BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE domains ADD COLUMN IF NOT EXISTS verify_token VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE domains ADD COLUMN IF NOT EXISTS verified_at BIGINT;
ALTER TABLE domains ADD COLUMN IF NOT EXISTS verified_method VARCHAR(16);

UPDATE domains
   SET verified = TRUE,
       verified_method = 'legacy',
       verified_at = (EXTRACT(EPOCH FROM clock_timestamp()) * 1000)::BIGINT
 WHERE NOT verified
   AND verified_method IS NULL
   AND lower(domain) NOT IN (
       'gmail.com', 'googlemail.com', 'outlook.com', 'hotmail.com', 'live.com',
       'msn.com', 'yahoo.com', 'ymail.com', 'aol.com', 'icloud.com', 'me.com',
       'mac.com', 'proton.me', 'protonmail.com', 'gmx.com', 'gmx.net', 'gmx.de',
       'zoho.com', 'yandex.ru', 'yandex.com', 'ya.ru', 'mail.ru', 'inbox.ru',
       'list.ru', 'bk.ru', 'internet.ru', 'rambler.ru', 'lenta.ru', 'autorambler.ru',
       'myrambler.ru', 'ro.ru'
   );

UPDATE domains
   SET verify_token = md5(random()::text || id::text || clock_timestamp()::text)
 WHERE verify_token = '';

-- Уникальность имени — только среди подтверждённых; один пользователь не
-- заявляет один домен дважды.
ALTER TABLE domains DROP CONSTRAINT IF EXISTS domains_domain_key;
CREATE UNIQUE INDEX IF NOT EXISTS domains_verified_domain_key ON domains (domain) WHERE verified;
CREATE UNIQUE INDEX IF NOT EXISTS domains_user_domain_key ON domains (user_id, domain);
