package feeds

import (
	"errors"
	"net/url"
	"time"
)

// Subscription describes a subscription and its persisted fetch metadata.
type Subscription struct {
	FolderID                   int64
	ID                         int64
	URL, Title, LastError      string
	CheckedAt                  *time.Time
	NextFetchAt, LastSuccessAt *time.Time
	TTLMinutes                 int64
	ETag, LastModified         string
}

// ValidateURL accepts HTTP and HTTPS URLs without credentials.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return errors.New("http または https のURLを入力してください")
	}
	return nil
}

// FetchResult carries metadata only after the full response has been saved.
// On failure the caller supplies the previously saved metadata.
type FetchResult struct {
	Title, ETag, LastModified string
	TTLMinutes                int64
	Err                       error
}

// ErrRefreshInProgress indicates another process holds the refresh lock.
var ErrRefreshInProgress = errors.New("feed refresh already in progress")
