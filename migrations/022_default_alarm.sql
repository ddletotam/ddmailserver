-- Add default alarm settings to calendar sources
ALTER TABLE calendar_sources ADD COLUMN IF NOT EXISTS default_alarm_enabled BOOLEAN DEFAULT false;
ALTER TABLE calendar_sources ADD COLUMN IF NOT EXISTS default_alarm_before INTEGER DEFAULT 15;
ALTER TABLE calendar_sources ADD COLUMN IF NOT EXISTS default_alarm_unit VARCHAR(10) DEFAULT 'minutes';
