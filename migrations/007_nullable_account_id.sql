-- Allow NULL account_id for local mail delivery (MX server)
-- Messages received via MX don't have an associated external account

ALTER TABLE messages ALTER COLUMN account_id DROP NOT NULL;
