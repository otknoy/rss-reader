package search

import (
	"context"
	"rss-reader/internal/articles"
)

type Service struct{ articles *articles.Store }

func New(store *articles.Store) *Service { return &Service{articles: store} }
func (s *Service) Find(ctx context.Context, q articles.Query) (articles.Result, error) {
	return s.articles.Search(ctx, q)
}
