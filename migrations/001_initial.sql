CREATE SCHEMA feeds;
CREATE SCHEMA articles;
CREATE TABLE feeds.subscriptions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    url text NOT NULL UNIQUE,
    title text NOT NULL,
    active boolean NOT NULL DEFAULT true,
    checked_at timestamptz,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE articles.entries (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id bigint NOT NULL DEFAULT 0,
    source_name text NOT NULL DEFAULT '手動登録',
    external_id text NOT NULL,
    url text NOT NULL,
    title text NOT NULL,
    body_text text NOT NULL DEFAULT '',
    body_html text NOT NULL DEFAULT '',
    published_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    is_read boolean NOT NULL DEFAULT false,
    favorite boolean NOT NULL DEFAULT false,
    UNIQUE (source_id, external_id)
);
CREATE INDEX entries_published ON articles.entries (published_at DESC, id DESC);
GRANT USAGE ON SCHEMA feeds TO feeds_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA feeds TO feeds_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA feeds TO feeds_app;
GRANT USAGE ON SCHEMA articles TO articles_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA articles TO articles_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA articles TO articles_app;
