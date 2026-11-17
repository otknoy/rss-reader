ALTER TABLE articles.entries
    ADD COLUMN full_body_html text NOT NULL DEFAULT '',
    ADD COLUMN full_body_text text NOT NULL DEFAULT '',
    ADD COLUMN full_source_url text NOT NULL DEFAULT '',
    ADD COLUMN full_fetched_at timestamptz;
