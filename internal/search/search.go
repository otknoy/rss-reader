package search

import (
	"context"
	"rss-reader/internal/articles"
)

type articleSearcher interface {
	Search(context.Context, articles.Query) (articles.Result, error)
}

// Service searches saved articles.
type Service struct{ articles articleSearcher }

// New creates a search service backed by the article store.
func New(store articleSearcher) *Service { return &Service{articles: store} }

// Find returns the articles matching the query.
func (s *Service) Find(ctx context.Context, q articles.Query) (articles.Result, error) {
	return s.articles.Search(ctx, q)
}
