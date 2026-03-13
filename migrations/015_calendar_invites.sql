-- Calendar invites support: attendees, organizer, account link

-- Link calendar source to email account for sending invites
ALTER TABLE calendar_sources ADD COLUMN account_id INTEGER REFERENCES accounts(id) ON DELETE SET NULL;
CREATE INDEX idx_calendar_sources_account ON calendar_sources(account_id);

-- Add organizer and status fields to events
ALTER TABLE calendar_events ADD COLUMN organizer_email VARCHAR(255);
ALTER TABLE calendar_events ADD COLUMN organizer_name VARCHAR(255);
ALTER TABLE calendar_events ADD COLUMN sequence INTEGER DEFAULT 0;
ALTER TABLE calendar_events ADD COLUMN status VARCHAR(50) DEFAULT 'CONFIRMED';

-- Attendees table
CREATE TABLE calendar_attendees (
    id SERIAL PRIMARY KEY,
    event_id INTEGER NOT NULL REFERENCES calendar_events(id) ON DELETE CASCADE,
    email VARCHAR(255) NOT NULL,
    name VARCHAR(255),
    role VARCHAR(50) DEFAULT 'REQ-PARTICIPANT',  -- CHAIR, REQ-PARTICIPANT, OPT-PARTICIPANT, NON-PARTICIPANT
    partstat VARCHAR(50) DEFAULT 'NEEDS-ACTION', -- NEEDS-ACTION, ACCEPTED, DECLINED, TENTATIVE
    rsvp BOOLEAN DEFAULT true,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(event_id, email)
);

CREATE INDEX idx_calendar_attendees_event ON calendar_attendees(event_id);
CREATE INDEX idx_calendar_attendees_email ON calendar_attendees(email);
