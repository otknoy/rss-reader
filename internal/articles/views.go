package articles

import (
	"context"
	"database/sql"
)

// RecordDisplay records each display once.
func (s *Store) RecordDisplay(ctx context.Context, id int64, viewID string) error {
	var exists bool
	err := s.db.QueryRowContext(ctx, `WITH target AS (
		SELECT id FROM articles.entries WHERE id=$1 AND deleted_at IS NULL FOR UPDATE
	), recorded AS (
		INSERT INTO articles.article_views (id, article_id, viewed_at)
		SELECT COALESCE(NULLIF($2, '')::uuid, gen_random_uuid()), id, now() FROM target
		ON CONFLICT (id) DO NOTHING
		RETURNING article_id, viewed_at
	), updated AS (
	UPDATE articles.entries AS e SET
		first_opened_at=COALESCE(e.first_opened_at, recorded.viewed_at),
		last_opened_at=GREATEST(e.last_opened_at, recorded.viewed_at)
	FROM recorded WHERE e.id=recorded.article_id
	)
	SELECT EXISTS (SELECT 1 FROM target)`, id, viewID).Scan(&exists)
	if err == nil && !exists {
		return sql.ErrNoRows
	}
	return err
}
