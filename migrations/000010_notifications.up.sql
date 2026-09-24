-- Outbound notifications (doc/notification.md).
--
-- Three tables, one per role in a delivery:
--
--   notification_channels  an administrator's provider configuration: which of the four
--                          adapters, its non-secret settings, and its credential encrypted
--                          with the same AES-GCM key that protects model-provider API keys.
--   notification_targets   one address a user can be reached at on one channel: a Telegram
--                          chat id, a Bark device key, an FCM registration token, an APNs
--                          device token. The address is a credential — anyone holding an
--                          FCM token can push to that device — so it is stored encrypted
--                          and only its hash and a masked hint are readable.
--   notification_messages  the delivery queue and its log. A message is claimed by a
--                          conditional update, sent outside any transaction, and then
--                          recorded, so a crash mid-send leaves a row that is due again
--                          rather than a delivery nobody can account for.
--
-- Deleting a channel removes its targets and their delivery history (CASCADE): a channel
-- an administrator deletes is a credential revoked, and a target whose credential is gone
-- cannot be delivered to or audited meaningfully.

CREATE TABLE IF NOT EXISTS notification_channels (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    provider TEXT NOT NULL CHECK (provider IN ('telegram', 'bark', 'fcm', 'apns')),
    -- Provider base URL. Empty means the adapter's own default (api.telegram.org,
    -- fcm.googleapis.com, the APNs host implied by environment).
    endpoint TEXT NOT NULL DEFAULT '',
    settings_json TEXT NOT NULL DEFAULT '{}',
    secret_ciphertext TEXT NOT NULL DEFAULT '',
    secret_hint TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    -- The last credential check an administrator ran, and what it said. NULL last_check_at
    -- means the channel was never checked; an empty last_check_status with a timestamp is
    -- not a state the application can write.
    last_check_at DATETIME,
    last_check_status TEXT NOT NULL DEFAULT '' CHECK (last_check_status IN ('', 'ok', 'failed')),
    last_error TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_notification_channels_status
    ON notification_channels(status, provider, updated_at DESC);

CREATE TABLE IF NOT EXISTS notification_targets (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    label TEXT NOT NULL DEFAULT '',
    address_ciphertext TEXT NOT NULL,
    -- sha256 of the plaintext address: it makes the one-registration-per-address rule
    -- exact without keeping a readable copy of a device token.
    address_hash TEXT NOT NULL,
    -- The masked address an administrator or user recognises the target by.
    address_hint TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled', 'invalid')),
    failure_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    last_sent_at DATETIME,
    revision INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    UNIQUE(user_id, channel_id, address_hash)
);

CREATE INDEX IF NOT EXISTS idx_notification_targets_user
    ON notification_targets(user_id, status);
CREATE INDEX IF NOT EXISTS idx_notification_targets_channel
    ON notification_targets(channel_id, status, created_at DESC);

CREATE TABLE IF NOT EXISTS notification_messages (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    target_id TEXT NOT NULL REFERENCES notification_targets(id) ON DELETE CASCADE,
    topic TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    body TEXT NOT NULL DEFAULT '',
    url TEXT NOT NULL DEFAULT '',
    payload_json TEXT NOT NULL DEFAULT '{}',
    -- queued  waiting for run_after
    -- sending claimed by a dispatcher; run_after is the claim's lease expiry, so a
    --         dispatcher that died mid-send leaves a row that becomes due again
    -- sent    the provider accepted it
    -- failed  the attempts ran out, or the provider will never accept it
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'sending', 'sent', 'failed')),
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 3,
    provider_message_id TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    run_after DATETIME NOT NULL,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    sent_at DATETIME
);

CREATE INDEX IF NOT EXISTS idx_notification_messages_due
    ON notification_messages(status, run_after);
CREATE INDEX IF NOT EXISTS idx_notification_messages_user
    ON notification_messages(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_notification_messages_channel
    ON notification_messages(channel_id, created_at DESC);
-- The log is bounded by pruning, and pruning only ever looks at finished rows.
CREATE INDEX IF NOT EXISTS idx_notification_messages_prune
    ON notification_messages(created_at) WHERE status IN ('sent', 'failed');

INSERT INTO schema_migrations(version, name, applied_at)
VALUES (10, 'notifications', CURRENT_TIMESTAMP);
