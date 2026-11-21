ALTER TABLE articles.entries ADD COLUMN last_opened_at timestamptz;

UPDATE articles.entries SET last_opened_at = first_opened_at
WHERE first_opened_at IS NOT NULL;

CREATE INDEX entries_history_idx ON articles.entries (last_opened_at DESC, id DESC)
WHERE last_opened_at IS NOT NULL AND deleted_at IS NULL;
