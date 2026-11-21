package articles

import (
	"context"
	"errors"
)

// FullContent is a readable version of the original page, kept separately from the feed.
type FullContent struct{ Title, HTML, Text, URL string }

// SaveFullContent preserves feed content and reading state when saving extracted content.
func (s *Store) SaveFullContent(ctx context.Context, id int64, content FullContent) error {
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

// PendingFullContent returns one bounded batch of articles awaiting extraction.
func (s *Store) PendingFullContent(ctx context.Context) (items []Article, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM articles.entries e`+fullContentJoin+`
 WHERE deleted_at IS NULL AND full_next_fetch_at<=now()
 AND (full_fetch_until IS NULL OR full_fetch_until<=now())
 ORDER BY full_next_fetch_at,id LIMIT 10`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ClaimFullContent atomically leases a fetch across worker replicas.
// An abandoned lease expires after two minutes, without holding a DB transaction during HTTP.
func (s *Store) ClaimFullContent(ctx context.Context, id int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE articles.entries SET full_fetch_until=now()+interval '2 minutes'
 WHERE id=$1 AND deleted_at IS NULL
 AND (full_fetch_until IS NULL OR full_fetch_until<=now())
 AND full_next_fetch_at<=now()`, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// FailFullContent releases a lease, retaining saved text and delaying automatic retries.
func (s *Store) FailFullContent(ctx context.Context, id int64, fetchErr error) error {
	canceled := errors.Is(fetchErr, context.Canceled)
	_, err := s.db.ExecContext(ctx, `UPDATE articles.entries SET full_fetch_until=NULL,
 full_next_fetch_at=CASE WHEN $3 THEN full_next_fetch_at WHEN full_next_fetch_at IS NOT NULL THEN now()+interval '30 minutes' ELSE NULL END,
 full_fetch_error=CASE WHEN $3 THEN full_fetch_error ELSE $2 END
 WHERE id=$1`, id, fetchErr.Error(), canceled)
	return err
}

// ContentStatus is a lightweight view used while an open article awaits automatic extraction.
type ContentStatus struct {
	Pending bool   `json:"pending"`
	Error   string `json:"error"`
}

// FullContentStatus reads extraction state without loading the stored article bodies.
func (s *Store) FullContentStatus(ctx context.Context, id int64) (status ContentStatus, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT full_next_fetch_at IS NOT NULL,full_fetch_error
 FROM articles.entries WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&status.Pending, &status.Error)
	return status, err
}
