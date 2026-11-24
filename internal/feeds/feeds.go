package feeds

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Feed describes a subscription and its persisted fetch metadata.
type Feed struct {
	FolderID                   int64
	ID                         int64
	URL, Title, LastError      string
	CheckedAt                  *time.Time
	NextFetchAt, LastSuccessAt *time.Time
	TTLMinutes                 int64
	ETag, LastModified         string
}

// Store persists feed subscriptions and fetch schedules.
type Store struct{ db *sql.DB }

// New creates a subscription store.
func New(db *sql.DB) *Store { return &Store{db: db} }

// ValidateURL accepts HTTP and HTTPS URLs without credentials.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return errors.New("http または https のURLを入力してください")
	}
	return nil
}

// Add subscribes to a URL or reactivates an existing subscription.
func (s *Store) Add(ctx context.Context, raw string) error {
	_, err := s.AddToFolder(ctx, raw, nil)
	return err
}

// AddToFolder registers a feed and optionally assigns it to a folder.
func (s *Store) AddToFolder(ctx context.Context, raw string, folder *int64) (int64, error) {
	raw = strings.TrimSpace(raw)
	if err := ValidateURL(raw); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO feeds.subscriptions(url,title,folder_id)
 VALUES($1,$1,NULLIF($2::bigint,0))
 ON CONFLICT(url) DO UPDATE SET active=true,
 folder_id=CASE WHEN $2 IS NULL THEN feeds.subscriptions.folder_id ELSE NULLIF($2,0) END,
 next_fetch_at=CASE WHEN feeds.subscriptions.active THEN feeds.subscriptions.next_fetch_at ELSE NULL END
 RETURNING id`, raw, folder).Scan(&id)
	return id, err
}

// Remove deactivates a subscription.
func (s *Store) Remove(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE feeds.subscriptions SET active=false WHERE id=$1 AND active`, id)
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

// List returns active subscriptions.
func (s *Store) List(ctx context.Context) ([]Feed, error) {
	return list(ctx, s.db, false, nil)
}

// ListDue returns active subscriptions whose next fetch is due.
func (s *Store) ListDue(ctx context.Context) ([]Feed, error) {
	return list(ctx, s.db, true, nil)
}

// ListDueFeed returns the specified subscription if it is active and due.
func (s *Store) ListDueFeed(ctx context.Context, id int64) ([]Feed, error) {
	return list(ctx, s.db, true, &id)
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func list(ctx context.Context, db queryer, dueOnly bool, id *int64) (out []Feed, err error) {
	query := `SELECT id, url, title, checked_at, last_error,
		ttl_minutes, next_fetch_at, last_success_at, etag, last_modified, COALESCE(folder_id, 0)
	FROM feeds.subscriptions
	WHERE active AND (NOT $1 OR next_fetch_at IS NULL OR next_fetch_at <= now())`
	args := []any{dueOnly}
	if id != nil {
		query += ` AND id = $2`
		args = append(args, *id)
	}
	rows, err := db.QueryContext(ctx, query+` ORDER BY next_fetch_at NULLS FIRST, id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	for rows.Next() {
		var f Feed
		if err := rows.Scan(
			&f.ID,
			&f.URL,
			&f.Title,
			&f.CheckedAt,
			&f.LastError,
			&f.TTLMinutes,
			&f.NextFetchAt,
			&f.LastSuccessAt,
			&f.ETag,
			&f.LastModified,
			&f.FolderID,
		); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FetchResult carries metadata only after the full response has been saved.
// On failure the caller supplies the previously saved metadata.
type FetchResult struct {
	Title, ETag, LastModified string
	TTLMinutes                int64
	Err                       error
}

// Record saves fetch metadata and schedules the next attempt.
func (s *Store) Record(ctx context.Context, id int64, result FetchResult, interval time.Duration) error {
	message := ""
	if result.Err != nil {
		message = result.Err.Error()
	}

	_, err := s.db.ExecContext(ctx, `UPDATE feeds.subscriptions SET
		checked_at = now(),
		last_error = $2,
		title = CASE WHEN $3 = '' THEN title ELSE $3 END,
		ttl_minutes = $4::bigint,
		etag = $5,
		last_modified = $6,
		next_fetch_at = now() + make_interval(
			secs => GREATEST($7::double precision, $4::bigint::double precision * 60)
		),
		last_success_at = CASE WHEN $8 THEN now() ELSE last_success_at END
	WHERE id = $1`,
		id, message, result.Title, result.TTLMinutes, result.ETag, result.LastModified,
		interval.Seconds(), result.Err == nil,
	)
	return err
}

// ErrRefreshInProgress indicates another process holds the refresh lock.
var ErrRefreshInProgress = errors.New("feed refresh already in progress")

// WithRefreshLock coordinates web, background polling and one-shot commands.
// A session lock needs a dedicated connection; never return a locked connection to the pool.
func (s *Store) WithRefreshLock(ctx context.Context, run func() error) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	var locked bool
	if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(7221902)`).Scan(&locked); err != nil {
		// A canceled query may have acquired the lock before its result was lost.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return err
	}
	if !locked {
		return ErrRefreshInProgress
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		unlockErr := conn.QueryRowContext(cleanup, `SELECT pg_advisory_unlock(7221902)`).Scan(&unlocked)
		if unlockErr != nil || !unlocked {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			if unlockErr == nil {
				unlockErr = errors.New("refresh lock was lost")
			}
			err = errors.Join(err, fmt.Errorf("release refresh lock: %w", unlockErr))
		}
	}()
	return run()
}

// Ping checks connectivity to the subscription database.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
