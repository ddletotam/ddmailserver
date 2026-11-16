-- A mailbox on a hosted domain becomes an account of its own
-- (docs/mailbox-accounts-plan.md): a users row with kind = 'mailbox', whose
-- login is the full address and whose password is the mailbox password. All
-- mail of the mailbox lives under that row's id, so an IMAP or SMTP session
-- that signs in with the address sees and sends only as that mailbox.
--
-- Such a row never signs in to the web, the client API or receives push.
-- mailboxes.principal_id NULL keeps a mailbox the old way: its mail goes to
-- the owner (mailboxes.user_id). Existing mailboxes are not converted here —
-- an administrator converts them one by one.
ALTER TABLE users ADD COLUMN IF NOT EXISTS kind VARCHAR(8) NOT NULL DEFAULT 'user';
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_kind_check;
ALTER TABLE users ADD CONSTRAINT users_kind_check CHECK (kind IN ('user', 'mailbox'));

ALTER TABLE mailboxes ADD COLUMN IF NOT EXISTS principal_id BIGINT REFERENCES users(id) ON DELETE SET NULL;
CREATE UNIQUE INDEX IF NOT EXISTS mailboxes_principal_id_key ON mailboxes (principal_id) WHERE principal_id IS NOT NULL;
