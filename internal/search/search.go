package search

import (
	"context"
	"rss-reader/internal/articles"
)

// Service searches saved articles.
type Service struct{ articles *articles.Store }

// New creates a search service backed by the article store.
func New(store *articles.Store) *Service { return &Service{articles: store} }

// Find returns the articles matching the query.
func (s *Service) Find(ctx context.Context, q articles.Query) (articles.Result, error) {
	return s.articles.Search(ctx, q)
}
