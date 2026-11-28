-- DKIM keys of users' own domains (docs/lunlun-bot-plan.md): made by the
-- server when the domain is added, so its DNS record can be shown right away.
-- The private key is encrypted with the server's encryption key. Keys of the
-- server's own domains stay files in dkim.key_dir.
CREATE TABLE IF NOT EXISTS dkim_keys (
    domain      VARCHAR(255) PRIMARY KEY,
    private_key TEXT NOT NULL,
    public_key  TEXT NOT NULL,   -- base64 DER, the p= of the DNS record
    created_at  BIGINT NOT NULL
);
