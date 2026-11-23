package feeds

import (
	"context"
	"database/sql"
)

// Inspect returns a bounded page of raw records from a feed-owned table.
func (s *Store) Inspect(ctx context.Context, table string, limit, offset int64) ([]byte, error) {
	var query, order string
	switch table {
	case "subscriptions":
		query = "SELECT * FROM feeds.subscriptions ORDER BY id DESC LIMIT $1 OFFSET $2"
		order = "id DESC"
	case "folders":
		query = "SELECT * FROM feeds.folders ORDER BY id DESC LIMIT $1 OFFSET $2"
		order = "id DESC"
	case "closed_folders":
		query = "SELECT * FROM feeds.closed_folders ORDER BY folder_id NULLS FIRST LIMIT $1 OFFSET $2"
		order = "folder_id NULLS FIRST"
	default:
		return nil, sql.ErrNoRows
	}
	var data []byte
	err := s.db.QueryRowContext(
		ctx,
		"SELECT COALESCE(jsonb_agg(t ORDER BY "+order+"), '[]'::jsonb) FROM ("+query+") AS t",
		limit,
		offset,
	).Scan(&data)
	return data, err
}
