-- Soft delete support for calendar events (for EAS deletion tracking)

ALTER TABLE calendar_events ADD COLUMN IF NOT EXISTS soft_deleted BOOLEAN DEFAULT false;
ALTER TABLE calendar_events ADD COLUMN IF NOT EXISTS soft_deleted_at TIMESTAMPTZ;

-- Index for efficient filtering
CREATE INDEX IF NOT EXISTS idx_calendar_events_soft_deleted ON calendar_events(soft_deleted);
CREATE INDEX IF NOT EXISTS idx_calendar_events_calendar_not_deleted ON calendar_events(calendar_id) WHERE soft_deleted = false;

-- Soft delete support for contacts
ALTER TABLE contacts ADD COLUMN IF NOT EXISTS soft_deleted BOOLEAN DEFAULT false;
ALTER TABLE contacts ADD COLUMN IF NOT EXISTS soft_deleted_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_contacts_soft_deleted ON contacts(soft_deleted);
CREATE INDEX IF NOT EXISTS idx_contacts_address_book_not_deleted ON contacts(address_book_id) WHERE soft_deleted = false;
