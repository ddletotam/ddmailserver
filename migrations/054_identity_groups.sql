-- 054: группы айдентик desktop-клиента (имя, цвет, подпись, порядок адресов,
-- адрес-отправитель, «не предлагать»).
--
-- Один документ на пользователя, устроен как dialog_folders (053): сервер
-- его не толкует — адреса собирает клиент, в том числе из учёток, которые
-- сервер не видит (IMAP напрямую). Сервер хранит JSON и раздаёт его всем
-- станциям пользователя.
--
-- version — оптимистичная блокировка, как у папок.
--
-- Идемпотентна.

CREATE TABLE IF NOT EXISTS identity_groups (
    user_id    BIGINT NOT NULL PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    doc        TEXT   NOT NULL,
    version    BIGINT NOT NULL,
    updated_at BIGINT NOT NULL
);
