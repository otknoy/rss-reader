package feeds

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"time"
)

type Feed struct {
	ID                    int64
	URL, Title, LastError string
	CheckedAt             *time.Time
}
type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db: db} }

func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return errors.New("http または https のURLを入力してください")
	}
	return nil
}
func (s *Store) Add(ctx context.Context, raw string) error {
	raw = strings.TrimSpace(raw)
	if err := ValidateURL(raw); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO feeds.subscriptions (url,title) VALUES ($1,$1) ON CONFLICT(url) DO UPDATE SET active=true`, raw)
	return err
}
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
func (s *Store) List(ctx context.Context) ([]Feed, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,url,title,checked_at,last_error FROM feeds.subscriptions WHERE active ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Feed
	for rows.Next() {
		var f Feed
		if err := rows.Scan(&f.ID, &f.URL, &f.Title, &f.CheckedAt, &f.LastError); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
func (s *Store) Record(ctx context.Context, id int64, title string, fetchErr error) error {
	message := ""
	if fetchErr != nil {
		message = fetchErr.Error()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE feeds.subscriptions SET checked_at=now(),last_error=$2,title=CASE WHEN $3='' THEN title ELSE $3 END WHERE id=$1`, id, message, title)
	return err
}
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
