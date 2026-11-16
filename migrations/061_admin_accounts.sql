-- Administrator accounts are separate from users (decision 2026-11-16,
-- docs/server-admin-split-plan.md): an account with is_admin manages the
-- server in /admin and has no mail of its own — it cannot sign in on IMAP,
-- SMTP, DAV, the client API or the user pages.
--
-- Until now is_admin was a flag on an ordinary user. A user who already has
-- mail (external accounts, domains, mailboxes) keeps it and stops being an
-- administrator; the installation gets a separate administrator with
-- `mailserver -create-admin <name>`.
UPDATE users SET is_admin = false
WHERE is_admin
  AND (EXISTS (SELECT 1 FROM accounts a WHERE a.user_id = users.id)
    OR EXISTS (SELECT 1 FROM domains d WHERE d.user_id = users.id)
    OR EXISTS (SELECT 1 FROM mailboxes m WHERE m.user_id = users.id));

-- What administrators did: who, what, to whom, when. Kept when the target is
-- deleted (no foreign key on target_id); admin_id is cleared if the
-- administrator account itself is removed.
CREATE TABLE IF NOT EXISTS admin_audit_log (
    id          BIGSERIAL PRIMARY KEY,
    admin_id    BIGINT REFERENCES users(id) ON DELETE SET NULL,
    admin_name  TEXT NOT NULL,
    action      TEXT NOT NULL,
    target_type TEXT NOT NULL DEFAULT '',
    target_id   BIGINT,
    details     TEXT NOT NULL DEFAULT '',
    ip          TEXT NOT NULL DEFAULT '',
    created_at  BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_admin_audit_log_created ON admin_audit_log (created_at DESC);
