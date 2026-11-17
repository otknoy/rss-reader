package articles

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"time"
)

// Article represents a saved article and its reading state.
type Article struct {
	ID, SourceID                                           int64
	ExternalID, SourceName, URL, Title, BodyText, BodyHTML string
	PublishedAt                                            time.Time
	IsRead, Favorite                                       bool
}

// Query selects articles by text, state, feed and page.
type Query struct {
	Text, View string
	FeedID     int64
	Page       int
}

// Result contains a page of articles and aggregate counts.
type Result struct {
	Items                    []Article
	Total, Unread, Favorites int
	Page, Pages              int
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

const columns = `id,source_id,external_id,source_name,url,title,body_text,body_html,published_at,is_read,favorite`

func scan(row interface{ Scan(...any) error }) (Article, error) {
	var a Article
	err := row.Scan(&a.ID, &a.SourceID, &a.ExternalID, &a.SourceName, &a.URL, &a.Title, &a.BodyText, &a.BodyHTML, &a.PublishedAt, &a.IsRead, &a.Favorite)
	return a, err
}

// Get returns an article that has not been deleted.
func (s *Store) Get(ctx context.Context, id int64) (Article, error) {
	return scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM articles.entries WHERE id=$1 AND deleted_at IS NULL`, id))
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

// Search returns matching articles and reading-state counts.
func (s *Store) Search(ctx context.Context, q Query) (r Result, err error) {
	r = Result{Page: q.Page}
	if r.Page < 1 {
		r.Page = 1
	}
	// Escape LIKE metacharacters so user queries are literal substring searches.
	pattern := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(strings.TrimSpace(q.Text)) + "%"
	where := ` WHERE deleted_at IS NULL AND ($1='%%' OR title ILIKE $1 OR body_text ILIKE $1) AND ($2::bigint=0 OR source_id=$2) AND ($3='' OR ($3='unread' AND NOT is_read) OR ($3='favorites' AND favorite))`
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM articles.entries`+where, pattern, q.FeedID, q.View).Scan(&r.Total); err != nil {
		return r, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE NOT is_read),count(*) FILTER(WHERE favorite) FROM articles.entries WHERE deleted_at IS NULL`).Scan(&r.Unread, &r.Favorites); err != nil {
		return r, err
	}
	r.Pages = (r.Total + 19) / 20
	if r.Pages < 1 {
		r.Pages = 1
	}
	if r.Page > r.Pages {
		r.Page = r.Pages
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM articles.entries`+where+` ORDER BY published_at DESC,id DESC LIMIT 20 OFFSET $4`, pattern, q.FeedID, q.View, (r.Page-1)*20)
	if err != nil {
		return r, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return r, err
		}
		r.Items = append(r.Items, a)
	}
	return r, rows.Err()
}

// Ping checks connectivity to the article database.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
