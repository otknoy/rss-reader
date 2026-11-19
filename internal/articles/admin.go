package articles

import "context"

// Inspect lists article metadata or returns a single full article record.
func (s *Store) Inspect(ctx context.Context, id, limit, offset int64) ([]byte, error) {
	var data []byte
	if id > 0 {
		err := s.db.QueryRowContext(ctx, "SELECT jsonb_build_array(to_jsonb(e)) FROM articles.entries e WHERE id=$1", id).Scan(&data)
		return data, err
	}
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(t ORDER BY t.id DESC), '[]'::jsonb) FROM (
		SELECT id, source_id, title, url, published_at, created_at, is_read, favorite, deleted_at,
			full_fetched_at, full_next_fetch_at, full_fetch_until, full_fetch_error
		FROM articles.entries ORDER BY id DESC LIMIT $1 OFFSET $2
	) AS t`, limit, offset).Scan(&data)
	return data, err
}
