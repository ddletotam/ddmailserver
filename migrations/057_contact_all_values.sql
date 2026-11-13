-- 057: все адреса и телефоны контакта; правка контакта без потерь.
--
-- До этой миграции у контакта было ровно три слота под адрес (email, email2,
-- email3) и три под телефон (phone, phone2, phone3): разбор vCard отбрасывал
-- четвёртый и дальше, API отдавал не больше трёх, а правка из клиента
-- пересобирала карточку из этих слотов и уносила урезанную версию во внешний
-- CardDAV (аудит iOS 2026-11-13, S-14).
--
--   contacts.emails / contacts.phones — все значения EMAIL / TEL карточки в
--     порядке vCard. Старые колонки остаются и по-прежнему заполняются первыми
--     тремя значениями: на них опираются откат на прежний бинарник и места,
--     которым хватает первых трёх.
--   contact_sync_queue.patch — правка из API (только изменённые поля). Воркер
--     обратной синхронизации берёт по ней свежую карточку с сервера (GET),
--     меняет в ней эти поля по строкам и пишет PUT с If-Match. NULL — в
--     очереди целая карточка (создание, PUT через наш CardDAV-сервер).
--
-- Заполнение массивов здесь — только из трёх старых колонок: больше SQL не
-- знает. Карточки, где слоты были заняты все три, сервер при старте
-- перечитывает из vcard_data (db.BackfillContactValues) и дописывает хвост.
--
-- Идемпотентна: повторный прогон не трогает уже заполненные строки.

ALTER TABLE contacts ADD COLUMN IF NOT EXISTS emails TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE contacts ADD COLUMN IF NOT EXISTS phones TEXT[] NOT NULL DEFAULT '{}';

UPDATE contacts
SET emails = ARRAY(
    SELECT v FROM unnest(ARRAY[email, email2, email3]) WITH ORDINALITY AS t(v, n)
    WHERE COALESCE(btrim(v), '') <> ''
    ORDER BY n)
WHERE cardinality(emails) = 0
  AND COALESCE(btrim(email), '') || COALESCE(btrim(email2), '') || COALESCE(btrim(email3), '') <> '';

UPDATE contacts
SET phones = ARRAY(
    SELECT v FROM unnest(ARRAY[phone, phone2, phone3]) WITH ORDINALITY AS t(v, n)
    WHERE COALESCE(btrim(v), '') <> ''
    ORDER BY n)
WHERE cardinality(phones) = 0
  AND COALESCE(btrim(phone), '') || COALESCE(btrim(phone2), '') || COALESCE(btrim(phone3), '') <> '';

ALTER TABLE contact_sync_queue ADD COLUMN IF NOT EXISTS patch JSONB;
