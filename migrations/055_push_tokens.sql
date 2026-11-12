-- 055: токены push-уведомлений устройств (APNs, HMS, FCM).
--
-- Клиенты в фоне не держат WebSocket: о новом письме им говорит push
-- (ios-behavior-contract.md §7, android-behavior-contract.md §7). Регистрация
-- одна на все платформы: kind — служба доставки, token — адрес устройства в ней.
--
-- Строка — пара (пользователь, устройство): на одном телефоне могут быть две
-- ddmail-учётки одного сервера, и каждая регистрирует тот же токен на себя.
-- Регистрация идемпотентна (повтор обновляет updated_at), выход из учётки
-- удаляет строку, токен, который служба назвала недействительным, удаляется
-- у всех пользователей сразу (индекс по (kind, token)).
--
-- environment — окружение APNs: токен отладочной сборки живёт только в
-- sandbox, релизной — только в production. У HMS и FCM пусто.
-- topic — bundle id приложения (apns-topic); пусто — тот, что в конфиге.
--
-- В push не уходит ничего, кроме факта «есть новые письма» (152-ФЗ: службы
-- доставки — серверы за рубежом); таблица хранит только адреса устройств.
--
-- Идемпотентна.

CREATE TABLE IF NOT EXISTS push_tokens (
    id           BIGSERIAL    PRIMARY KEY,
    user_id      BIGINT       NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind         VARCHAR(8)   NOT NULL CHECK (kind IN ('apns', 'hms', 'fcm')),
    token        TEXT         NOT NULL,
    environment  VARCHAR(16)  NOT NULL DEFAULT '' CHECK (environment IN ('', 'sandbox', 'production')),
    topic        VARCHAR(255) NOT NULL DEFAULT '',
    created_at   BIGINT       NOT NULL,
    updated_at   BIGINT       NOT NULL,
    last_sent_at BIGINT,
    UNIQUE (user_id, kind, token)
);

CREATE INDEX IF NOT EXISTS push_tokens_by_token ON push_tokens (kind, token);
