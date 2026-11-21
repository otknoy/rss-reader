package articles

import "context"

// Inspect lists article metadata or returns a single full article record.
func (s *Store) Inspect(ctx context.Context, id, limit, offset int64) ([]byte, error) {
	var data []byte
	if id > 0 {
		err := s.db.QueryRowContext(ctx, `SELECT jsonb_build_array(to_jsonb(e))
			FROM articles.entries e WHERE id=$1`, id).Scan(&data)
		return data, err
	}
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(t ORDER BY t.id DESC), '[]'::jsonb) FROM (
		SELECT id, source_id, title, url, published_at, created_at, is_read, favorite, deleted_at,
			first_opened_at, last_opened_at, full_next_fetch_at, full_fetch_until, full_fetch_error
		FROM articles.entries ORDER BY id DESC LIMIT $1 OFFSET $2
	) AS t`, limit, offset).Scan(&data)
	return data, err
}

// InspectFullContents lists saved full text metadata or returns a single record.
func (s *Store) InspectFullContents(ctx context.Context, id, limit, offset int64) ([]byte, error) {
	var data []byte
	if id > 0 {
		err := s.db.QueryRowContext(ctx, `SELECT jsonb_build_array(to_jsonb(f))
			FROM articles.full_contents f WHERE article_id=$1`, id).Scan(&data)
		return data, err
	}
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(f ORDER BY f.article_id DESC), '[]'::jsonb) FROM (
		SELECT article_id, source_url, fetched_at FROM articles.full_contents
		ORDER BY article_id DESC LIMIT $1 OFFSET $2
	) AS f`, limit, offset).Scan(&data)
	return data, err
}

// InspectViews lists viewing events from newest to oldest.
func (s *Store) InspectViews(ctx context.Context, limit, offset int64) ([]byte, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(v ORDER BY v.viewed_at DESC, v.id DESC), '[]'::jsonb) FROM (
		SELECT id, article_id, viewed_at FROM articles.article_views
		ORDER BY viewed_at DESC, id DESC LIMIT $1 OFFSET $2
	) AS v`, limit, offset).Scan(&data)
	return data, err
}
