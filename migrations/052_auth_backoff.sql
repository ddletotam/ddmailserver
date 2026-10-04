-- 052: пауза между попытками входа, когда провайдер не принимает учётные данные.
--
-- Повод (прод): у внешнего аккаунта Яндекс 360 отозвали пароль приложения, и
-- сервер с тех пор каждую минуту логинился им заново — IMAP-синк, IDLE, CalDAV,
-- CardDAV, каждый своим циклом («LOGIN invalid credentials or IMAP is
-- disabled», «REPORT returned 401», «401 Unauthorized»). Так провайдер может
-- принять это за подбор пароля и заблокировать учётку.
--
-- Строка существует, пока учётные данные «субъекта» не проходят: число неудач
-- подряд, когда была первая и последняя, когда разрешена следующая попытка и
-- текст последнего отказа. Удачный вход или правка учётных данных строку
-- удаляют. Субъекты:
--   account_imap    — accounts.id, вход по IMAP (синк, IDLE, flag/delete sync);
--   account_smtp    — accounts.id, отправка через SMTP аккаунта (outbox);
--   calendar_source — calendar_sources.id (CalDAV: синк и обратная запись);
--   contact_source  — contact_sources.id (CardDAV: синк и обратная запись).
-- IMAP и SMTP раздельно: у аккаунта два набора учётных данных, и общий счётчик
-- качался бы между «IMAP прошёл — сброс» и «SMTP не прошёл — пауза».
--
-- notified_at — когда о переходе в состояние сообщили пользователю (push по
-- WebSocket); 0 — ещё не сообщали. Ставится один раз на переход.
--
-- Внешнего ключа на субъект нет (он в одной из трёх таблиц); удалённые
-- субъекты подчищает планировщик раз в сутки. user_id — для выборки по
-- пользователю и каскада при удалении пользователя.
--
-- Идемпотентна.

CREATE TABLE IF NOT EXISTS auth_backoff (
    subject_kind     VARCHAR(20) NOT NULL,
    subject_id       BIGINT      NOT NULL,
    user_id          BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    failures         INTEGER     NOT NULL DEFAULT 0,
    first_failure_at BIGINT      NOT NULL DEFAULT 0,
    last_failure_at  BIGINT      NOT NULL DEFAULT 0,
    next_attempt_at  BIGINT      NOT NULL DEFAULT 0,
    last_error       TEXT        NOT NULL DEFAULT '',
    notified_at      BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (subject_kind, subject_id)
);

CREATE INDEX IF NOT EXISTS idx_auth_backoff_user ON auth_backoff(user_id);
