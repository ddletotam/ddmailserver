-- 056: статус доставки исходящего письма.
--
-- Копия отправленного письма теперь ложится в «Отправленные» в момент, когда
-- сервер принял письмо (POST /send, SMTP submission), а не после доставки:
-- беседа с новым адресатом появляется сразу. Доставка идёт позже, и её исход
-- живёт на этой копии:
--
--   delivery_status  ''          — неизвестно (письма до этой миграции,
--                                  входящие); клиенты считают доставленным;
--                    'pending'   — в очереди / отправляется (в том числе
--                                  между повторными попытками);
--                    'delivered' — принято сервером получателя (для отправки
--                                  через внешнюю учётку — SMTP провайдера);
--                    'failed'    — не доставлено хотя бы одному адресату.
--   delivery_reason  код причины (internal/outbox: unreachable,
--                    domain_not_found, …); текст для человека — из i18n.
--                    У 'pending' — причина последней неудачной попытки.
--   delivery_failed  адреса, которым доставить не удалось (через запятую).
--
-- Сырая ошибка SMTP/сети остаётся только в журнале и в
-- outbox_messages.last_error.
--
-- outbox_messages.message_id — Message-ID письма в очереди: им строка очереди
-- связана с копией в «Отправленных» (идентичность письма — (user_id,
-- Message-ID), см. 041/042). Пусто у строк, принятых до этой миграции: их
-- Message-ID фиксируется при следующей попытке отправки.
--
-- Статус для старых писем не выдумывается: колонки получают пустое значение.
--
-- Идемпотентна.

ALTER TABLE messages ADD COLUMN IF NOT EXISTS delivery_status VARCHAR(16) NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN IF NOT EXISTS delivery_reason VARCHAR(32) NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN IF NOT EXISTS delivery_failed TEXT NOT NULL DEFAULT '';

ALTER TABLE outbox_messages ADD COLUMN IF NOT EXISTS message_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_outbox_user_message_id
    ON outbox_messages (user_id, message_id)
    WHERE message_id <> '';

-- Журнал изменений (043) пишет UPDATE только по колонкам, от которых зависит
-- вид у клиента. Статус доставки теперь среди них: смена статуса должна дойти
-- до /changes, иначе клиент на журнале не перерисует пузырь.
DROP TRIGGER IF EXISTS trg_message_change_upd ON messages;

CREATE TRIGGER trg_message_change_upd
    AFTER UPDATE ON messages
    FOR EACH ROW
    WHEN (
        OLD.seen        IS DISTINCT FROM NEW.seen        OR
        OLD.flagged     IS DISTINCT FROM NEW.flagged     OR
        OLD.answered    IS DISTINCT FROM NEW.answered    OR
        OLD.deleted     IS DISTINCT FROM NEW.deleted     OR
        OLD.soft_deleted IS DISTINCT FROM NEW.soft_deleted OR
        OLD.is_spam     IS DISTINCT FROM NEW.is_spam     OR
        OLD.folder_id   IS DISTINCT FROM NEW.folder_id   OR
        OLD.delivery_status IS DISTINCT FROM NEW.delivery_status OR
        OLD.delivery_reason IS DISTINCT FROM NEW.delivery_reason OR
        OLD.delivery_failed IS DISTINCT FROM NEW.delivery_failed
    )
    EXECUTE FUNCTION record_message_change();
