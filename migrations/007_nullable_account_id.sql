-- Allow NULL account_id for local mail delivery (MX server)
-- Messages received via MX don't have an associated external account

ALTER TABLE messages ALTER COLUMN account_id DROP NOT NULL;

-- Mail sent from a local domain has no external account either. Production got
-- this by hand; recorded here so an empty database matches it.
ALTER TABLE outbox_messages ALTER COLUMN account_id DROP NOT NULL;
