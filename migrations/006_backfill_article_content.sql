-- Queue previously saved articles without overwriting completed content or retry schedules.
UPDATE articles.entries
SET full_next_fetch_at=now()
WHERE deleted_at IS NULL
  AND full_fetched_at IS NULL
  AND full_next_fetch_at IS NULL;
