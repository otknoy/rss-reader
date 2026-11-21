CREATE TABLE articles.full_contents (
    article_id bigint PRIMARY KEY REFERENCES articles.entries(id) ON DELETE CASCADE,
    body_html text NOT NULL,
    body_text text NOT NULL,
    source_url text NOT NULL,
    fetched_at timestamptz NOT NULL
);

INSERT INTO articles.full_contents (article_id, body_html, body_text, source_url, fetched_at)
SELECT id, full_body_html, full_body_text, full_source_url, full_fetched_at
FROM articles.entries
WHERE full_fetched_at IS NOT NULL;

GRANT SELECT, INSERT, UPDATE ON articles.full_contents TO articles_app;

ALTER TABLE articles.entries
    DROP COLUMN full_body_html,
    DROP COLUMN full_body_text,
    DROP COLUMN full_source_url,
    DROP COLUMN full_fetched_at;
