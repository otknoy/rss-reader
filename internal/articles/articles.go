package articles

import (
	"errors"
	"net/url"
	"time"
)

// Article represents a saved article and its reading state.
type Article struct {
	ID, SourceID                                           int64
	ExternalID, SourceName, URL, Title, BodyText, BodyHTML string
	PublishedAt, LastOpenedAt                              time.Time
	IsRead, Favorite                                       bool
	FullBodyHTML, FullBodyText, FullSourceURL              string
	FullFetchedAt                                          *time.Time
	FullFetchError                                         string
}

// NormalizeForSave validates the article URL and fills missing identity and display fields.
func NormalizeForSave(a Article) (Article, error) {
	u, err := url.Parse(a.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return Article{}, errors.New("http または https の記事URLを入力してください")
	}
	if a.Title == "" {
		a.Title = a.URL
	}
	if a.ExternalID == "" {
		a.ExternalID = a.URL
	}
	if a.SourceName == "" {
		a.SourceName = "手動登録"
	}
	return a, nil
}
