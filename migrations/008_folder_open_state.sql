CREATE TABLE feeds.closed_folders (
    folder_id bigint REFERENCES feeds.folders(id) ON DELETE CASCADE,
    UNIQUE NULLS NOT DISTINCT (folder_id)
);
GRANT SELECT, INSERT, DELETE ON feeds.closed_folders TO feeds_app;
