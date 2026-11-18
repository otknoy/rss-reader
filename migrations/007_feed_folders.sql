CREATE TABLE feeds.folders (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL UNIQUE CHECK (btrim(name) <> '')
);
ALTER TABLE feeds.subscriptions
    ADD COLUMN folder_id bigint REFERENCES feeds.folders(id) ON DELETE SET NULL;
CREATE INDEX subscriptions_folder ON feeds.subscriptions(folder_id);
GRANT SELECT, INSERT, UPDATE, DELETE ON feeds.folders TO feeds_app;
GRANT USAGE, SELECT ON SEQUENCE feeds.folders_id_seq TO feeds_app;
