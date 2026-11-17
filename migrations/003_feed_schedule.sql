ALTER TABLE feeds.subscriptions
    ADD COLUMN ttl_minutes bigint NOT NULL DEFAULT 0 CHECK (ttl_minutes >= 0),
    ADD COLUMN next_fetch_at timestamptz,
    ADD COLUMN last_success_at timestamptz,
    ADD COLUMN etag text NOT NULL DEFAULT '',
    ADD COLUMN last_modified text NOT NULL DEFAULT '';
CREATE INDEX subscriptions_due ON feeds.subscriptions (next_fetch_at, id) WHERE active;
