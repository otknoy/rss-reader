CREATE TABLE articles.article_views (
    id uuid PRIMARY KEY,
    article_id bigint NOT NULL REFERENCES articles.entries(id) ON DELETE CASCADE,
    viewed_at timestamptz NOT NULL
);

CREATE INDEX article_views_article_time_idx
    ON articles.article_views (article_id, viewed_at DESC);

GRANT SELECT, INSERT ON articles.article_views TO articles_app;
