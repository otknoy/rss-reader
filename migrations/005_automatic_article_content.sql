ALTER TABLE articles.entries
    ADD COLUMN full_next_fetch_at timestamptz,
    ADD COLUMN full_fetch_until timestamptz,
    ADD COLUMN full_fetch_error text NOT NULL DEFAULT '';
CREATE INDEX entries_pending_content ON articles.entries (full_next_fetch_at, id)
    WHERE full_next_fetch_at IS NOT NULL AND deleted_at IS NULL;
