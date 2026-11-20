ALTER TABLE articles.entries ADD COLUMN first_opened_at timestamptz;

UPDATE articles.entries
SET full_next_fetch_at=NULL, full_fetch_until=NULL, full_fetch_error=''
WHERE full_fetched_at IS NULL AND full_next_fetch_at IS NOT NULL;
