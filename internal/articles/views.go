package articles

import (
	"context"
)

// RecordDisplay records each display once and schedules extraction if needed.
func (s *Store) RecordDisplay(ctx context.Context, id int64, viewID string) (item Article, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return item, err
	}
	defer tx.Rollback()

	// Lock the article first so the next statement sees any concurrent full-content save.
	if err := tx.QueryRowContext(ctx, `SELECT id, url FROM articles.entries
		WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&item.ID, &item.URL); err != nil {
		return item, err
	}

	err = tx.QueryRowContext(ctx, `WITH recorded AS (
		INSERT INTO articles.article_views (id, article_id, viewed_at)
		VALUES (COALESCE(NULLIF($2, '')::uuid, gen_random_uuid()), $1, now())
		ON CONFLICT (id) DO NOTHING
		RETURNING article_id, viewed_at
	), updated AS (
		UPDATE articles.entries AS e SET
			first_opened_at=COALESCE(e.first_opened_at, recorded.viewed_at),
			last_opened_at=GREATEST(e.last_opened_at, recorded.viewed_at),
			full_next_fetch_at = CASE
				WHEN NOT EXISTS (
					SELECT 1 FROM articles.full_contents f WHERE f.article_id = e.id
				)
				THEN COALESCE(e.full_next_fetch_at, recorded.viewed_at)
			END
		FROM recorded WHERE e.id=recorded.article_id
		RETURNING e.id, (e.full_next_fetch_at IS NOT NULL) AS pending
	)
	SELECT COALESCE((SELECT pending FROM updated), false)`, id, viewID).Scan(&item.FullContentPending)
	if err != nil {
		return item, err
	}
	return item, tx.Commit()
}
