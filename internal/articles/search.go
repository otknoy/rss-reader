package articles

import "time"

// Query selects articles by text, state, feed and page.
type Query struct {
	Text, View        string
	FeedID            int64
	FeedIDs           []int64
	Page              int
	NewestFirst       bool
	AnchorID          int64
	BeforeAnchor      bool
	HistoryBefore     time.Time
	AnchorPublishedAt time.Time
	ForwardOnly       bool // Omit totals and detect the next page with one extra row.
}

// Result contains a page of articles and aggregate counts.
type Result struct {
	Items                []Article
	Total                int
	Page, Pages          int
	HasPrevious, HasNext bool
}
