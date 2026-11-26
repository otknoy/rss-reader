package postgres

import (
	"context"
	"database/sql"
	"errors"
	"rss-reader/internal/articles"
)

// SaveFullContent preserves feed content and reading state when saving extracted content.
func (s *Store) SaveFullContent(ctx context.Context, id int64, content articles.FullContent) error {
	result, err := s.db.ExecContext(ctx, `WITH saved AS (
		UPDATE articles.entries SET
			full_next_fetch_at=NULL,full_fetch_until=NULL,full_fetch_error='',
			title=CASE WHEN title=url AND $5<>'' THEN $5 ELSE title END
		WHERE id=$1 AND deleted_at IS NULL
		RETURNING id
	)
	INSERT INTO articles.full_contents (article_id, body_html, body_text, source_url, fetched_at)
	SELECT id, $2, $3, $4, now() FROM saved
	ON CONFLICT (article_id) DO UPDATE SET
		body_html=EXCLUDED.body_html,body_text=EXCLUDED.body_text,
		source_url=EXCLUDED.source_url,fetched_at=EXCLUDED.fetched_at`,
		id, content.HTML, content.Text, content.URL, content.Title)
	return affected(result, err)
}

// ClaimFullContent leases a first attempt or an explicit retry and returns the article URL.
func (s *Store) ClaimFullContent(ctx context.Context, id int64, retry bool) (string, error) {
	var url string
	err := s.db.QueryRowContext(ctx, `UPDATE articles.entries e
		SET full_fetch_until=now()+interval '2 minutes'
		WHERE id=$1 AND deleted_at IS NULL
		  AND (full_fetch_until IS NULL OR full_fetch_until<=now())
		  AND (full_fetch_error <> '') = $2
		  AND NOT EXISTS (SELECT 1 FROM articles.full_contents f WHERE f.article_id=e.id)
		RETURNING url`, id, retry).Scan(&url)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return url, err
}

// FailFullContent releases the lease and records the failure for display.
func (s *Store) FailFullContent(ctx context.Context, id int64, fetchErr error) error {
	_, err := s.db.ExecContext(ctx, `UPDATE articles.entries SET full_fetch_until=NULL,
		full_next_fetch_at=NULL, full_fetch_error=$2 WHERE id=$1`, id, fetchErr.Error())
	return err
}

// FullContentStatus reads extraction state without loading the stored article bodies.
func (s *Store) FullContentStatus(ctx context.Context, id int64) (status articles.ContentStatus, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM articles.full_contents f WHERE f.article_id=e.id),
 COALESCE(full_fetch_until > now(), false), full_fetch_error
 FROM articles.entries e WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&status.Fetched, &status.Pending, &status.Error)
	return status, err
}
