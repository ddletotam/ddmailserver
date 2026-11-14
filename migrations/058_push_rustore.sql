-- 058: служба доставки push RuStore (Android-сборка rustore).
--
-- Android выходит двумя сборками (android-behavior-contract.md §1, §7):
-- hms — Huawei Push Kit, rustore — RuStore Push. Ограничение на kind из 055
-- пускало только apns, hms и fcm; rustore добавляется к ним. fcm остаётся
-- допустимым значением (строк с ним нет, отправителя тоже).
--
-- Идемпотентна: ограничение пересоздаётся под известным именем.

ALTER TABLE push_tokens DROP CONSTRAINT IF EXISTS push_tokens_kind_check;
ALTER TABLE push_tokens ADD CONSTRAINT push_tokens_kind_check
    CHECK (kind IN ('apns', 'hms', 'rustore', 'fcm'));
