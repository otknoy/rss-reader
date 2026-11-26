// Package workspace assembles the data needed for an article list.
package workspace

import (
	"context"
	"errors"
	"rss-reader/internal/articles"
	"rss-reader/internal/feeds"
)

var ErrFolderNotFound = errors.New("folder not found")

type subscriptions interface {
	List(context.Context) ([]feeds.Feed, error)
	Folders(context.Context) ([]feeds.Folder, error)
	ClosedFolders(context.Context) (map[int64]bool, error)
	ActiveIDsInFolder(context.Context, int64) ([]int64, error)
}

type articleSearch interface {
	Find(context.Context, articles.Query) (articles.Result, error)
}

type unreadCounts interface {
	UnreadByFeed(context.Context) (map[int64]int, error)
}

type Service struct {
	subscriptions subscriptions
	search        articleSearch
	unread        unreadCounts
}

func New(subscriptions subscriptions, search articleSearch, unread unreadCounts) *Service {
	return &Service{subscriptions: subscriptions, search: search, unread: unread}
}

type ArticleList struct {
	Result articles.Result
	Sidebar
}

type Sidebar struct {
	Feeds          []feeds.Feed
	Folders        []feeds.Folder
	Closed         map[int64]bool
	FolderName     string
	FeedsByFolder  map[int64][]feeds.Feed
	UnreadByFeed   map[int64]int
	UnreadByFolder map[int64]int
}

// Load returns an article page and, for a full page, its sidebar data.
// A nil folderID leaves the folder scope unrestricted. ID 0 selects unfiled feeds.
func (s *Service) Load(ctx context.Context, query articles.Query, folderID *int64) (ArticleList, error) {
	if query.ForwardOnly {
		if folderID != nil {
			ids, err := s.subscriptions.ActiveIDsInFolder(ctx, *folderID)
			if err != nil {
				return ArticleList{}, err
			}
			query.FeedIDs = ids
		}
		result, err := s.search.Find(ctx, query)
		return ArticleList{Result: result}, err
	}

	sidebar, err := s.loadSidebar(ctx)
	if err != nil {
		return ArticleList{}, err
	}
	if folderID != nil {
		for _, folder := range sidebar.Folders {
			if folder.ID == *folderID {
				sidebar.FolderName = folder.Name
				break
			}
		}
		if *folderID != 0 && sidebar.FolderName == "" {
			return ArticleList{}, ErrFolderNotFound
		}
		list := sidebar.FeedsByFolder[*folderID]
		query.FeedIDs = make([]int64, 0, len(list))
		for _, feed := range list {
			query.FeedIDs = append(query.FeedIDs, feed.ID)
		}
	}

	result, err := s.search.Find(ctx, query)
	if err != nil {
		return ArticleList{}, err
	}
	sidebar.UnreadByFeed, err = s.unread.UnreadByFeed(ctx)
	if err != nil {
		return ArticleList{}, err
	}
	sidebar.UnreadByFolder = make(map[int64]int)
	for _, feed := range sidebar.Feeds {
		sidebar.UnreadByFolder[feed.FolderID] += sidebar.UnreadByFeed[feed.ID]
	}
	return ArticleList{Result: result, Sidebar: sidebar}, nil
}

func (s *Service) loadSidebar(ctx context.Context) (Sidebar, error) {
	list, err := s.subscriptions.List(ctx)
	if err != nil {
		return Sidebar{}, err
	}
	folders, err := s.subscriptions.Folders(ctx)
	if err != nil {
		return Sidebar{}, err
	}
	closed, err := s.subscriptions.ClosedFolders(ctx)
	if err != nil {
		return Sidebar{}, err
	}
	grouped := make(map[int64][]feeds.Feed)
	for _, feed := range list {
		grouped[feed.FolderID] = append(grouped[feed.FolderID], feed)
	}
	return Sidebar{Feeds: list, Folders: folders, Closed: closed, FeedsByFolder: grouped}, nil
}
