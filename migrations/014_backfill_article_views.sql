-- Recover the distinct viewing timestamps still stored on read articles.
INSERT INTO articles.article_views (id, article_id, viewed_at)
SELECT gen_random_uuid(), e.id, known.viewed_at
FROM articles.entries AS e
CROSS JOIN LATERAL (
    SELECT e.first_opened_at AS viewed_at
    UNION
    SELECT e.last_opened_at
) AS known
WHERE e.is_read
  AND known.viewed_at IS NOT NULL
  AND NOT EXISTS (
      SELECT 1
      FROM articles.article_views AS v
      WHERE v.article_id = e.id AND v.viewed_at = known.viewed_at
  );
