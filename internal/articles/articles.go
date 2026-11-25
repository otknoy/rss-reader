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
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO articles.entries (
			source_id, external_id, source_name, url, title, body_text, body_html, published_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (source_id, external_id) DO UPDATE SET
			source_name = EXCLUDED.source_name,
			url = EXCLUDED.url,
			title = EXCLUDED.title,
			body_text = EXCLUDED.body_text,
			body_html = EXCLUDED.body_html,
			published_at = CASE WHEN $9 THEN EXCLUDED.published_at ELSE entries.published_at END,
			deleted_at = CASE WHEN EXCLUDED.source_id = 0 THEN NULL ELSE entries.deleted_at END
		WHERE (entries.source_name, entries.url, entries.title, entries.body_text, entries.body_html)
			IS DISTINCT FROM (EXCLUDED.source_name, EXCLUDED.url, EXCLUDED.title, EXCLUDED.body_text, EXCLUDED.body_html)
			OR ($9 AND entries.published_at IS DISTINCT FROM EXCLUDED.published_at)
			OR (EXCLUDED.source_id = 0 AND entries.deleted_at IS NOT NULL)`,
		a.SourceID,
		a.ExternalID,
		a.SourceName,
		a.URL,
		a.Title,
		a.BodyText,
		a.BodyHTML,
		a.PublishedAt,
		hasPublished,
	)
	return err
}

const columns = `e.id,e.source_id,e.external_id,e.source_name,e.url,e.title,e.body_text,e.body_html,e.published_at,e.is_read,e.favorite,
 COALESCE(f.body_html,''),COALESCE(f.body_text,''),COALESCE(f.source_url,''),f.fetched_at,e.full_fetch_error`
const fullContentJoin = ` LEFT JOIN articles.full_contents f ON f.article_id=e.id`

func scan(row interface{ Scan(...any) error }) (Article, error) {
	var a Article
	err := row.Scan(
		&a.ID,
		&a.SourceID,
		&a.ExternalID,
		&a.SourceName,
		&a.URL,
		&a.Title,
		&a.BodyText,
		&a.BodyHTML,
		&a.PublishedAt,
		&a.IsRead,
		&a.Favorite,
		&a.FullBodyHTML,
		&a.FullBodyText,
		&a.FullSourceURL,
		&a.FullFetchedAt,
		&a.FullFetchError,
	)
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
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE articles.entries SET `+field+`= $2 WHERE id=$1 AND deleted_at IS NULL`,
		id,
		value,
	)
	return affected(result, err)
}

// MarkRead marks the specified non-deleted articles as read and returns the number changed.
func (s *Store) MarkRead(ctx context.Context, ids []int64) (int64, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE articles.entries SET is_read=true
		WHERE id=ANY($1::bigint[]) AND deleted_at IS NULL AND NOT is_read`, ids)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
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
