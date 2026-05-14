-- Per-user weights for each spam analyzer category. Missing rows mean the
-- default weight (1.0). The analyzer multiplies the category's accumulated
-- score by `weight` before adding it to the total — setting a weight to 0
-- effectively disables the category, identical to user_disabled_spam_checks
-- (we keep both because the toggle is a single click while weights need a
-- value entry).
CREATE TABLE IF NOT EXISTS spam_check_weights (
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    check_name TEXT NOT NULL,
    weight DOUBLE PRECISION NOT NULL DEFAULT 1.0,
    updated_at BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, check_name)
);

CREATE INDEX IF NOT EXISTS idx_spam_check_weights_user ON spam_check_weights(user_id);

-- Per-rule check exclusion: when a whitelist rule matches, the analyzer can
-- skip a specific subset of checks for that sender/domain instead of the
-- usual all-or-nothing behaviour. JSON array of category names (same names
-- used in user_disabled_spam_checks); '[]' = full whitelist as before.
ALTER TABLE user_spam_rules
    ADD COLUMN IF NOT EXISTS excluded_checks TEXT NOT NULL DEFAULT '[]';
