-- Per-account aliases for recipient validation.
-- A message is considered legitimately addressed to this account if any
-- address in its To/Cc headers matches the account's email or one of its aliases.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS aliases TEXT NOT NULL DEFAULT '';
