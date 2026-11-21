package articles

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"time"
)

// Article represents a saved article and its reading state.
type Article struct {
	ID, SourceID                                           int64
	ExternalID, SourceName, URL, Title, BodyText, BodyHTML string
	PublishedAt, LastOpenedAt                              time.Time
	IsRead, Favorite                                       bool
	FullBodyHTML, FullBodyText, FullSourceURL              string
	FullFetchedAt                                          *time.Time
	FullContentPending                                     bool
	FullFetchError                                         string
}

// Store persists articles in the articles database.
type Store struct{ db *sql.DB }

// New creates an article store.
func New(db *sql.DB) *Store { return &Store{db: db} }

// Save inserts or updates an article while preserving its reading state.
func (s *Store) Save(ctx context.Context, a Article) error {
	u, err := url.Parse(a.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return errors.New("http または https の記事URLを入力してください")
	}
	if a.Title == "" {
		a.Title = a.URL
	}
	if a.ExternalID == "" {
		a.ExternalID = a.URL
	}
	if a.SourceName == "" {
		a.SourceName = "手動登録"
	}
	hasPublished := !a.PublishedAt.IsZero()
	if !hasPublished {
		a.PublishedAt = time.Now()
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO articles.entries(source_id,external_id,source_name,url,title,body_text,body_html,published_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(source_id,external_id) DO UPDATE SET source_name=EXCLUDED.source_name,url=EXCLUDED.url,title=EXCLUDED.title,body_text=EXCLUDED.body_text,body_html=EXCLUDED.body_html,published_at=CASE WHEN $9 THEN EXCLUDED.published_at ELSE entries.published_at END,deleted_at=CASE WHEN EXCLUDED.source_id=0 THEN NULL ELSE entries.deleted_at END`, a.SourceID, a.ExternalID, a.SourceName, a.URL, a.Title, a.BodyText, a.BodyHTML, a.PublishedAt, hasPublished)
	return err
}

const columns = `e.id,e.source_id,e.external_id,e.source_name,e.url,e.title,e.body_text,e.body_html,e.published_at,e.is_read,e.favorite,
 COALESCE(f.body_html,''),COALESCE(f.body_text,''),COALESCE(f.source_url,''),f.fetched_at,(e.full_next_fetch_at IS NOT NULL),e.full_fetch_error`
const fullContentJoin = ` LEFT JOIN articles.full_contents f ON f.article_id=e.id`

func scan(row interface{ Scan(...any) error }) (Article, error) {
	var a Article
	err := row.Scan(&a.ID, &a.SourceID, &a.ExternalID, &a.SourceName, &a.URL, &a.Title, &a.BodyText, &a.BodyHTML, &a.PublishedAt, &a.IsRead, &a.Favorite, &a.FullBodyHTML, &a.FullBodyText, &a.FullSourceURL, &a.FullFetchedAt, &a.FullContentPending, &a.FullFetchError)
	return a, err
}

// Get returns an article that has not been deleted.
func (s *Store) Get(ctx context.Context, id int64) (Article, error) {
	return scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM articles.entries e`+fullContentJoin+` WHERE e.id=$1 AND e.deleted_at IS NULL`, id))
}

// Open marks an article as read and reports whether that changed its state.
func (s *Store) Open(ctx context.Context, id int64) (Article, bool, error) {
	article, err := scan(s.db.QueryRowContext(ctx, `WITH opened AS (
		UPDATE articles.entries SET is_read=true
		WHERE id=$1 AND deleted_at IS NULL AND NOT is_read RETURNING *
	)
	SELECT `+columns+` FROM opened e`+fullContentJoin, id))
	if errors.Is(err, sql.ErrNoRows) {
		article, err = s.Get(ctx, id)
		return article, false, err
	}
	return article, err == nil, err
}

// SetState updates the read or favorite state of an article.
func (s *Store) SetState(ctx context.Context, id int64, kind string, value bool) error {
	field := ""
	switch kind {
	case "read":
		field = "is_read"
	case "favorite":
		field = "favorite"
	default:
		return errors.New("unknown state")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE articles.entries SET `+field+`= $2 WHERE id=$1 AND deleted_at IS NULL`, id, value)
	return affected(result, err)
}

// MarkRead marks the specified non-deleted articles as read in one update.
func (s *Store) MarkRead(ctx context.Context, ids []int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE articles.entries SET is_read=true
		WHERE id=ANY($1::bigint[]) AND deleted_at IS NULL AND NOT is_read`, ids)
	return err
}

func affected(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Delete marks an article as deleted.
func (s *Store) Delete(ctx context.Context, id int64) error {
	r, e := s.db.ExecContext(ctx, `UPDATE articles.entries SET deleted_at=now() WHERE id=$1 AND deleted_at IS NULL`, id)
	return affected(r, e)
}

// UnreadByFeed returns unread counts for articles from feeds.
func (s *Store) UnreadByFeed(ctx context.Context) (counts map[int64]int, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source_id, count(*) FROM articles.entries
		WHERE source_id > 0 AND deleted_at IS NULL AND NOT is_read GROUP BY source_id`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	counts = make(map[int64]int)
	for rows.Next() {
		var id int64
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		counts[id] = count
	}
	return counts, rows.Err()
}

// Ping checks connectivity to the article database.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

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
			full_next_fetch_at=CASE WHEN NOT EXISTS (SELECT 1 FROM articles.full_contents f WHERE f.article_id=e.id) THEN COALESCE(e.full_next_fetch_at, recorded.viewed_at) END
		FROM recorded WHERE e.id=recorded.article_id
		RETURNING e.id, (e.full_next_fetch_at IS NOT NULL) AS pending
	)
	SELECT COALESCE((SELECT pending FROM updated), false)`, id, viewID).Scan(&item.FullContentPending)
	if err != nil {
		return item, err
	}
	return item, tx.Commit()
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
