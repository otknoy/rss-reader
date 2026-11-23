package articles

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Query selects articles by text, state, feed and page.
type Query struct {
	Text, View    string
	FeedID        int64
	FeedIDs       []int64
	Page          int
	NewestFirst   bool
	AnchorID      int64
	BeforeAnchor  bool
	HistoryBefore time.Time
}

// Result contains a page of articles and aggregate counts.
type Result struct {
	Items                []Article
	Total                int
	Page, Pages          int
	HasPrevious, HasNext bool
}

const pageSize = 20

const listColumns = `id,source_id,source_name,url,title,published_at,is_read,favorite`

// Search returns list metadata and the total number of matches. Get loads an article's body.
func (s *Store) Search(ctx context.Context, q Query) (r Result, err error) {
	r = Result{Page: q.Page}
	if r.Page < 1 {
		r.Page = 1
	}

	// Escape LIKE metacharacters so user queries are literal substring searches.
	pattern := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(strings.TrimSpace(q.Text)) + "%"
	where := ` WHERE deleted_at IS NULL
		AND (
			$1 = '%%' OR title ILIKE $1 OR body_text ILIKE $1
			OR EXISTS (
				SELECT 1 FROM articles.full_contents f
				WHERE f.article_id = entries.id AND f.body_text ILIKE $1
			)
		)
		AND ($2::bigint = 0 OR source_id = $2)
		AND ($4::bigint[] IS NULL OR source_id = ANY($4::bigint[]))
		AND (
			$3 = ''
			OR ($3 = 'unread' AND NOT is_read)
			OR ($3 = 'read' AND is_read)
			OR ($3 = 'favorites' AND favorite)
			OR ($3 = 'unread-favorites' AND NOT is_read AND favorite)
		)`
	args := []any{pattern, q.FeedID, q.View, q.FeedIDs}

	history := q.View == "history"
	if history {
		where += " AND last_opened_at IS NOT NULL"
		args[2] = ""
	}

	descending := q.NewestFirst != (q.AnchorID > 0 && q.BeforeAnchor)
	order, compare := "published_at ASC, id ASC", ">"
	if descending {
		order, compare = "published_at DESC, id DESC", "<"
	}
	offset := 0
	limit := pageSize
	if history {
		order = "last_opened_at DESC, id DESC"
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM articles.entries"+where, args...).Scan(&r.Total); err != nil {
			return r, err
		}
		if !q.HistoryBefore.IsZero() && q.AnchorID > 0 {
			where += " AND (last_opened_at,id) < ($5,$6)"
			args = append(args, q.HistoryBefore, q.AnchorID)
		}
		limit++
	} else if q.AnchorID > 0 {
		// The boundary row can be read already. Its sort key still anchors the next page.
		boundary := "(published_at,id) " + compare + " (SELECT published_at,id FROM articles.entries WHERE id=$5)"
		args = append(args, q.AnchorID)
		var onRequestedSide, onOtherSide int
		count := "SELECT count(*), count(*) FILTER (WHERE " + boundary + "), count(*) FILTER (WHERE NOT (" + boundary + "))"
		if err := s.db.QueryRowContext(ctx, count+" FROM articles.entries"+where, args...).Scan(&r.Total, &onRequestedSide, &onOtherSide); err != nil {
			return r, err
		}
		r.HasPrevious, r.HasNext = onOtherSide > 0, onRequestedSide > pageSize
		if q.BeforeAnchor {
			r.HasPrevious, r.HasNext = r.HasNext, r.HasPrevious
		}
		where += " AND " + boundary
	} else {
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM articles.entries`+where, args...).Scan(&r.Total); err != nil {
			return r, err
		}
		r.Pages = max(1, (r.Total+pageSize-1)/pageSize)
		r.Page = min(r.Page, r.Pages)
		r.HasPrevious, r.HasNext = r.Page > 1, r.Page < r.Pages
		offset = (r.Page - 1) * pageSize
	}

	args = append(args, limit, offset)
	projection := listColumns
	if history {
		projection += ",last_opened_at"
	}
	paging := fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	rows, err := s.db.QueryContext(ctx, `SELECT `+projection+` FROM articles.entries`+where+` ORDER BY `+order+paging, args...)
	if err != nil {
		return r, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	for rows.Next() {
		var a Article
		dest := []any{
			&a.ID,
			&a.SourceID,
			&a.SourceName,
			&a.URL,
			&a.Title,
			&a.PublishedAt,
			&a.IsRead,
			&a.Favorite,
		}
		if history {
			dest = append(dest, &a.LastOpenedAt)
		}
		if err := rows.Scan(dest...); err != nil {
			return r, err
		}
		r.Items = append(r.Items, a)
	}

	if history && len(r.Items) > pageSize {
		r.HasNext = true
		r.Items = r.Items[:pageSize]
	}
	if !history && q.AnchorID > 0 && q.BeforeAnchor {
		slices.Reverse(r.Items)
	}
	return r, rows.Err()
}
