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
	FullBodyHTML, FullBodyText, FullSourceURL              string
	FullFetchedAt                                          *time.Time
	FullContentPending                                     bool
	FullFetchError                                         string
}

// Query selects articles by text, state, feed and page.
type Query struct {
	Text, View string
	FeedID     int64
	FeedIDs    []int64
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
	_, err = s.db.ExecContext(ctx, `INSERT INTO articles.entries(source_id,external_id,source_name,url,title,body_text,body_html,published_at,full_next_fetch_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,now()) ON CONFLICT(source_id,external_id) DO UPDATE SET source_name=EXCLUDED.source_name,url=EXCLUDED.url,title=EXCLUDED.title,body_text=EXCLUDED.body_text,body_html=EXCLUDED.body_html,published_at=CASE WHEN $9 THEN EXCLUDED.published_at ELSE entries.published_at END,deleted_at=CASE WHEN EXCLUDED.source_id=0 THEN NULL ELSE entries.deleted_at END`, a.SourceID, a.ExternalID, a.SourceName, a.URL, a.Title, a.BodyText, a.BodyHTML, a.PublishedAt, hasPublished)
	return err
}

const columns = `id,source_id,external_id,source_name,url,title,body_text,body_html,published_at,is_read,favorite,full_body_html,full_body_text,full_source_url,full_fetched_at,(full_next_fetch_at IS NOT NULL),full_fetch_error`

func scan(row interface{ Scan(...any) error }) (Article, error) {
	var a Article
	err := row.Scan(&a.ID, &a.SourceID, &a.ExternalID, &a.SourceName, &a.URL, &a.Title, &a.BodyText, &a.BodyHTML, &a.PublishedAt, &a.IsRead, &a.Favorite, &a.FullBodyHTML, &a.FullBodyText, &a.FullSourceURL, &a.FullFetchedAt, &a.FullContentPending, &a.FullFetchError)
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
	where := ` WHERE deleted_at IS NULL AND ($1='%%' OR title ILIKE $1 OR body_text ILIKE $1 OR full_body_text ILIKE $1) AND ($2::bigint=0 OR source_id=$2) AND ($4::bigint[] IS NULL OR source_id=ANY($4::bigint[])) AND ($3='' OR ($3='unread' AND NOT is_read) OR ($3='favorites' AND favorite))`
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM articles.entries`+where, pattern, q.FeedID, q.View, q.FeedIDs).Scan(&r.Total); err != nil {
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
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM articles.entries`+where+` ORDER BY published_at DESC,id DESC LIMIT 20 OFFSET $5`, pattern, q.FeedID, q.View, q.FeedIDs, (r.Page-1)*20)
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

// FullContent is a readable version of the original page, kept separately from the feed.
type FullContent struct{ Title, HTML, Text, URL string }

// SaveFullContent preserves feed content and reading state when saving extracted content.
func (s *Store) SaveFullContent(ctx context.Context, id int64, content FullContent) error {
	result, err := s.db.ExecContext(ctx, `UPDATE articles.entries SET
 full_body_html=$2,full_body_text=$3,full_source_url=$4,full_fetched_at=now(),full_next_fetch_at=NULL,full_fetch_until=NULL,full_fetch_error='',
 title=CASE WHEN title=url AND $5<>'' THEN $5 ELSE title END
 WHERE id=$1 AND deleted_at IS NULL`, id, content.HTML, content.Text, content.URL, content.Title)
	return affected(result, err)
}

// PendingFullContent returns one bounded batch of articles awaiting extraction.
func (s *Store) PendingFullContent(ctx context.Context) (items []Article, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM articles.entries
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
