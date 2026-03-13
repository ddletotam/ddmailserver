-- Add last_error column to calendar_sources for tracking sync errors
ALTER TABLE calendar_sources ADD COLUMN last_error TEXT;
