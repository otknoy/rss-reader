package main

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"rss-reader/internal/articles"
	"rss-reader/internal/feeds"
	"rss-reader/internal/ingestion"
	"rss-reader/internal/reading"
	"rss-reader/internal/workspace"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

//go:embed page.html
var pageHTML string

//go:embed reader.js
var readerJS string

var page = template.Must(template.New("page").Funcs(template.FuncMap{
	"richArticle": richArticle,
	"articleURL":  articleURL,
	"date": func(t time.Time) string {
		return t.In(time.FixedZone("JST", 9*60*60)).Format("2006.01.02 15:04")
	},
	"excerpt": func(s string) string {
		r := []rune(s)
		if len(r) > 180 {
			return string(r[:180]) + "…"
		}
		return s
	},
}).Parse(pageHTML))

type application struct {
	feeds         *feeds.Store
	articles      *articles.Store
	subscriptions *feeds.SubscriptionService
	workspace     *workspace.Service
	reading       *reading.Service
	fetcher       *ingestion.Service
}
type view struct {
	FeedsByFolder                                             map[int64][]feeds.Subscription
	ClosedFolders                                             map[int64]bool
	FolderUnread, FeedUnread                                  map[int64]int
	Folders                                                   []feeds.Folder
	Folder, FolderName                                        string
	UnreadOnly, FavoritesOnly                                 bool
	UnreadURL, FavoritesURL                                   string
	NewestFirst                                               bool
	OldestURL, NewestURL                                      string
	Feeds                                                     []feeds.Subscription
	Result                                                    articles.Result
	Query, Filter, Message, ReturnTo, ListURL, Previous, Next string
	FeedID                                                    int64
	Detail                                                    *articles.Article
}

func (a *application) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("GET /admin/database", a.database)
	mux.HandleFunc("GET /assets/reader.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		if _, err := w.Write([]byte(readerJS)); err != nil {
			log.Printf("write reader script: %v", err)
		}
	})
	mux.HandleFunc("GET /articles/{id}", a.detail)
	mux.HandleFunc("POST /feeds", a.addFeed)
	mux.HandleFunc("POST /folders/{operation}", a.changeFolder)
	mux.HandleFunc("POST /folders/{operation}/{id}", a.changeFolder)
	mux.HandleFunc("POST /folders/open/{id}", a.setFolderOpen)
	mux.HandleFunc("POST /folders/order", a.setFolderOrder)
	mux.HandleFunc("POST /feeds/import", a.importOPML)
	mux.HandleFunc("GET /feeds/export.opml", a.exportOPML)
	mux.HandleFunc("POST /feeds/{id}/remove", a.removeFeed)
	mux.HandleFunc("POST /refresh", a.refresh)
	mux.HandleFunc("POST /articles", a.addArticle)
	mux.HandleFunc("POST /articles/mark-read", a.markRead)
	mux.HandleFunc("POST /articles/{id}/state", a.state)
	mux.HandleFunc("POST /articles/{id}/open", a.openArticle)
	mux.HandleFunc("GET /articles/{id}/content-status", a.contentStatus)
	mux.HandleFunc("POST /articles/{id}/content-status", a.contentStatus)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if a.feeds.Ping(ctx) != nil || a.articles.Ping(ctx) != nil {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		if _, err := w.Write([]byte("ok")); err != nil {
			log.Printf("write health response: %v", err)
		}
	})
	protected := http.NewCrossOriginProtection().Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; img-src https: http:; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		limit := int64(256 << 10)
		if r.Method == http.MethodPost && r.URL.Path == "/feeds/import" {
			limit = feeds.MaxOPMLBytes + (64 << 10)
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "入力が大きすぎるか不正です", 400)
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		protected.ServeHTTP(w, r.WithContext(ctx))
	})
}

func fail(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, workspace.ErrFolderNotFound) {
		http.Error(w, "見つかりません", 404)
		return
	}
	log.Printf("request failed: %v", err)
	http.Error(w, "処理に失敗しました。ログを確認してください。", 500)
}

func render(w http.ResponseWriter, v view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.Execute(w, v); err != nil {
		log.Printf("render: %v", err)
	}
}

func returnTo(r *http.Request) string {
	raw := r.FormValue("return_to")
	u, err := url.Parse(raw)
	if err != nil ||
		u.IsAbs() ||
		u.Host != "" ||
		!strings.HasPrefix(raw, "/") ||
		strings.HasPrefix(raw, "//") ||
		strings.ContainsAny(raw, "\\\r\n") {
		return "/"
	}
	return u.RequestURI()
}

func redirect(w http.ResponseWriter, r *http.Request, message string) {
	target := returnTo(r)
	u, _ := url.Parse(target)
	q := u.Query()
	q.Del("message")
	if message != "" {
		q.Set("message", message)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func id(r *http.Request) (int64, error) {
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || n < 1 {
		return 0, sql.ErrNoRows
	}
	return n, nil
}

func listFilter(value string) string {
	switch value {
	case "all", "unread", "read", "favorites", "unread-favorites", "history":
		return value
	default:
		return "unread"
	}
}

type listPagination struct {
	page          int
	anchorID      int64
	beforeAnchor  bool
	historyBefore time.Time
	publishedAt   time.Time
}

func parseListPagination(q url.Values, filter string, anchoredList bool) listPagination {
	var pagination listPagination
	pagination.page, _ = strconv.Atoi(q.Get("page"))
	switch {
	case filter == "history":
		pagination.historyBefore, _ = time.Parse(time.RFC3339Nano, q.Get("opened_before"))
		pagination.anchorID, _ = strconv.ParseInt(q.Get("after"), 10, 64)
	case anchoredList:
		pagination.publishedAt, _ = time.Parse(time.RFC3339Nano, q.Get("published_at"))
		pagination.anchorID, _ = strconv.ParseInt(q.Get("before"), 10, 64)
		pagination.beforeAnchor = pagination.anchorID > 0
		if !pagination.beforeAnchor {
			pagination.anchorID, _ = strconv.ParseInt(q.Get("after"), 10, 64)
		}
	}
	return pagination
}

// paginationLinks clears pagination parameters from q for subsequent links.
func paginationLinks(q url.Values, filter string, anchoredList bool, anchorID int64, result articles.Result) (string, string) {
	prev, next := "", ""
	q.Del("published_at")
	if filter == "history" {
		q.Del("after")
		q.Del("opened_before")
		if result.HasNext {
			last := result.Items[len(result.Items)-1]
			q.Set("after", strconv.FormatInt(last.ID, 10))
			q.Set("opened_before", last.LastOpenedAt.UTC().Format(time.RFC3339Nano))
			next = "/?" + q.Encode()
			q.Del("after")
			q.Del("opened_before")
		}
	} else if anchoredList {
		q.Del("page")
		q.Del("before")
		q.Del("after")
		if len(result.Items) > 0 {
			if result.HasPrevious {
				first := result.Items[0]
				q.Set("before", strconv.FormatInt(first.ID, 10))
				q.Set("published_at", first.PublishedAt.UTC().Format(time.RFC3339Nano))
				prev = "/?" + q.Encode()
				q.Del("before")
				q.Del("published_at")
			}
			if result.HasNext {
				last := result.Items[len(result.Items)-1]
				q.Set("after", strconv.FormatInt(last.ID, 10))
				q.Set("published_at", last.PublishedAt.UTC().Format(time.RFC3339Nano))
				next = "/?" + q.Encode()
				q.Del("after")
				q.Del("published_at")
			}
		} else if anchorID > 0 && result.Total > 0 {
			// Concurrent changes can empty an anchored page. Offer the beginning of the list.
			prev = "/?" + q.Encode()
		}
	} else {
		if result.HasPrevious {
			q.Set("page", strconv.Itoa(result.Page-1))
			prev = "/?" + q.Encode()
		}
		if result.HasNext {
			q.Set("page", strconv.Itoa(result.Page+1))
			next = "/?" + q.Encode()
		}
	}
	q.Del("page")
	return prev, next
}

func listControlLinks(q url.Values, filter string, unreadOnly bool) (favoritesOnly bool, unreadURL, favoritesURL, oldestURL, newestURL string) {
	favoritesOnly = filter == "favorites" || filter == "unread-favorites"
	filterURL := func(unread, favorite bool) string {
		switch {
		case unread && favorite:
			q.Set("view", "unread-favorites")
		case unread:
			q.Set("view", "unread")
		case favorite:
			q.Set("view", "favorites")
		default:
			q.Set("view", "all")
		}
		if len(q) == 0 {
			return "/"
		}
		return "/?" + q.Encode()
	}
	unreadURL = filterURL(!unreadOnly, favoritesOnly)
	favoritesURL = filterURL(unreadOnly, !favoritesOnly)
	q.Set("view", filter)
	q.Del("sort")
	oldestURL = "/"
	if len(q) > 0 {
		oldestURL += "?" + q.Encode()
	}
	q.Set("sort", "newest")
	newestURL = "/?" + q.Encode()
	return favoritesOnly, unreadURL, favoritesURL, oldestURL, newestURL
}

func (a *application) base(r *http.Request, articlePage bool) (view, error) {
	q := r.URL.Query()
	feedID, _ := strconv.ParseInt(q.Get("feed"), 10, 64)
	if feedID < 0 {
		feedID = 0
	}

	filter := listFilter(q.Get("view"))
	queryFilter := filter
	if filter == "all" {
		queryFilter = ""
	}
	unreadOnly := filter == "unread" || filter == "unread-favorites"
	anchoredList := unreadOnly || filter == "favorites"
	pagination := parseListPagination(q, filter, anchoredList)
	if filter == "history" {
		q.Del("page")
		q.Del("before")
	}

	folder := q.Get("folder")
	var folderID *int64
	if folder != "" {
		id, err := strconv.ParseInt(folder, 10, 64)
		if err != nil || id < 0 || strconv.FormatInt(id, 10) != folder {
			return view{}, sql.ErrNoRows
		}
		folderID = &id
	}
	newestFirst := filter != "history" && q.Get("sort") == "newest"
	if !newestFirst {
		q.Del("sort")
	}

	list, err := a.workspace.Load(r.Context(), articles.Query{
		Text:   q.Get("q"),
		View:   queryFilter,
		FeedID: feedID,
		Page:   pagination.page,

		NewestFirst:       newestFirst,
		AnchorID:          pagination.anchorID,
		BeforeAnchor:      pagination.beforeAnchor,
		HistoryBefore:     pagination.historyBefore,
		AnchorPublishedAt: pagination.publishedAt,
		ForwardOnly:       articlePage,
	}, folderID)
	if err != nil {
		return view{}, err
	}
	q.Del("message")
	q.Del("article")
	q.Del("added")
	q.Del("existing")
	q.Del("skipped")
	back := "/"
	if len(q) > 0 {
		back += "?" + q.Encode()
	}
	result := list.Result
	prev, next := paginationLinks(q, filter, anchoredList, pagination.anchorID, result)
	if articlePage {
		return view{Result: result, Filter: filter, Query: r.URL.Query().Get("q"), ListURL: back, ReturnTo: back, Next: next}, nil
	}
	folderName := list.FolderName
	if folder == "0" {
		folderName = "未分類"
	}

	favoritesOnly, unreadURL, favoritesURL, oldestURL, newestURL := listControlLinks(q, filter, unreadOnly)

	message := ""
	switch r.URL.Query().Get("message") {
	case "feed-queued":
		message = "フィードを登録しました。次回の自動更新で記事を取得します。"
	case "feed-initial-error":
		message = "フィードを登録しましたが、初回取得に失敗しました。購読管理の取得状況をご確認ください。"
	case "saved":
		message = "保存しました"
	case "updated":
		message = "記事を更新しました"
	case "fetch-error":
		message = "取得できなかったフィードがあります。「購読を管理」の取得状況をご確認ください。"
	case "opml-imported":
		message = fmt.Sprintf(
			"OPML: %d件登録・%d件重複・%d件スキップ",
			opmlCount(r.URL.Query().Get("added")),
			opmlCount(r.URL.Query().Get("existing")),
			opmlCount(r.URL.Query().Get("skipped")),
		)
	case "removed":
		message = "解除しました。取得済みの記事は残しています。"
	}

	return view{
		ClosedFolders: list.Closed,
		FolderUnread:  list.UnreadByFolder,
		FeedUnread:    list.UnreadByFeed,
		Folders:       list.Folders,
		Folder:        folder,
		FolderName:    folderName,
		UnreadOnly:    unreadOnly,
		FavoritesOnly: favoritesOnly,
		UnreadURL:     unreadURL,
		FavoritesURL:  favoritesURL,
		NewestFirst:   newestFirst,
		OldestURL:     oldestURL,
		NewestURL:     newestURL,
		Feeds:         list.Feeds,
		FeedsByFolder: list.FeedsByFolder,
		Result:        result,
		Query:         r.URL.Query().Get("q"),
		Filter:        filter,
		FeedID:        feedID,

		ReturnTo: back,
		ListURL:  back,
		Previous: prev,
		Next:     next,
		Message:  message,
	}, nil
}

func (a *application) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Accept")
	articlePage := r.Header.Get("Accept") == "text/vnd.rss-reader.article-page+html"
	v, err := a.base(r, articlePage)
	if err != nil {
		fail(w, err)
		return
	}
	if articlePage {
		w.Header().Set("Content-Type", "text/vnd.rss-reader.article-page+html; charset=utf-8")
		if err := page.ExecuteTemplate(w, "article-page", v); err != nil {
			log.Printf("render article page: %v", err)
		}
		return
	}
	if raw := r.URL.Query().Get("article"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 {
			http.Error(w, "記事の指定が不正です", http.StatusBadRequest)
			return
		}
		item, err := a.articles.Get(r.Context(), n)
		if err != nil {
			fail(w, err)
			return
		}
		v.Detail = &item
		v.ReturnTo = articleURL(v.ListURL, n)
	}
	render(w, v)
}

func (a *application) detail(w http.ResponseWriter, r *http.Request) {
	n, err := id(r)
	if err != nil {
		fail(w, err)
		return
	}
	item, err := a.articles.Get(r.Context(), n)
	if err != nil {
		fail(w, err)
		return
	}
	v, err := a.base(r, false)
	if err != nil {
		fail(w, err)
		return
	}
	v.Detail = &item
	v.ReturnTo = articleURL(v.ListURL, n)
	render(w, v)
}

func (a *application) addFeed(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.FormValue("url"))
	if err := feeds.ValidateURL(raw); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var folder *int64
	if rawFolder := r.FormValue("folder"); rawFolder != "" {
		n, err := strconv.ParseInt(rawFolder, 10, 64)
		if err != nil || n < 0 {
			http.Error(w, "フォルダーの指定が不正です", http.StatusBadRequest)
			return
		}
		folder = &n
	}
	result, err := a.subscriptions.Subscribe(r.Context(), raw, folder)
	if err != nil {
		if errors.Is(err, feeds.ErrFolderNotFound) {
			http.Error(w, "フォルダーがありません", http.StatusBadRequest)
			return
		}
		fail(w, err)
		return
	}
	message := "saved"
	if err := result.InitialFetchError; err != nil {
		if errors.Is(err, feeds.ErrRefreshInProgress) {
			message = "feed-queued"
		} else {
			log.Printf("initial feed refresh %d: %v", result.ID, err)
			message = "feed-initial-error"
		}
	}
	redirect(w, r, message)
}

func (a *application) removeFeed(w http.ResponseWriter, r *http.Request) {
	n, err := id(r)
	if err == nil {
		err = a.feeds.Remove(r.Context(), n)
	}
	if err != nil {
		fail(w, err)
		return
	}
	redirect(w, r, "removed")
}

func (a *application) refresh(w http.ResponseWriter, r *http.Request) {
	message := "updated"
	if err := a.fetcher.Refresh(r.Context()); err != nil {
		if errors.Is(err, feeds.ErrRefreshInProgress) {
			http.Error(w, "取得処理を実行中です。しばらくお待ちください。", http.StatusConflict)
			return
		}
		log.Printf("fetch: %v", err)
		message = "fetch-error"
	}
	redirect(w, r, message)
}

func (a *application) addArticle(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.FormValue("url"))
	if err := feeds.ValidateURL(raw); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	err := a.articles.Save(r.Context(), articles.Article{
		URL:      raw,
		Title:    strings.TrimSpace(r.FormValue("title")),
		BodyText: strings.TrimSpace(r.FormValue("body")),
	})
	if err != nil {
		fail(w, err)
		return
	}
	redirect(w, r, "saved")
}

func (a *application) openArticle(w http.ResponseWriter, r *http.Request) {
	n, err := id(r)
	if err != nil {
		fail(w, err)
		return
	}
	item, changed, err := a.reading.Open(r.Context(), n)
	if err != nil {
		fail(w, err)
		return
	}
	listURL := articleURL(returnTo(r), 0)
	var reader bytes.Buffer
	if err := page.ExecuteTemplate(&reader, "reader-pane", view{
		Detail: &item, ListURL: listURL, ReturnTo: articleURL(listURL, n),
	}); err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	payload := struct {
		HTML     string `json:"html"`
		SourceID string `json:"sourceId"`
		Changed  bool   `json:"changed"`
	}{reader.String(), strconv.FormatInt(item.SourceID, 10), changed}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("write article pane: %v", err)
	}
}

func (a *application) state(w http.ResponseWriter, r *http.Request) {
	kind, value := r.FormValue("kind"), r.FormValue("value")
	if (kind != "read" && kind != "favorite") || (value != "true" && value != "false") {
		http.Error(w, "不正な操作です", 400)
		return
	}
	n, err := id(r)
	if err == nil {
		err = a.articles.SetState(r.Context(), n, kind, value == "true")
	}
	if err != nil {
		fail(w, err)
		return
	}
	redirect(w, r, "")
}

func (a *application) markRead(w http.ResponseWriter, r *http.Request) {
	values := r.PostForm["id"]
	if len(values) == 0 {
		http.Error(w, "記事を指定してください", http.StatusBadRequest)
		return
	}
	ids := make([]int64, len(values))
	for i, value := range values {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 1 {
			http.Error(w, "記事の指定が不正です", http.StatusBadRequest)
			return
		}
		ids[i] = n
	}
	count, err := a.articles.MarkRead(r.Context(), ids)
	if err != nil {
		fail(w, err)
		return
	}
	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(struct {
			Count int64 `json:"count"`
		}{count})
		return
	}
	redirect(w, r, "")
}

// articleURL keeps the current list context while changing the selected article.
func articleURL(listURL string, id int64) string {
	u, err := url.Parse(listURL)
	if err != nil {
		u = &url.URL{}
	}
	q := u.Query()
	q.Del("message")
	if id > 0 {
		q.Set("article", strconv.FormatInt(id, 10))
	} else {
		q.Del("article")
	}
	if len(q) == 0 {
		return "/"
	}
	return "/?" + q.Encode()
}

func (a *application) contentStatus(w http.ResponseWriter, r *http.Request) {
	n, err := id(r)
	if err != nil {
		fail(w, err)
		return
	}
	if r.Method == http.MethodPost {
		if r.PostForm.Get("retry") == "true" {
			a.reading.RetryContent(r.Context(), n)
			redirect(w, r, "")
			return
		}
		viewID := r.Header.Get("X-Article-View-ID")
		if viewID != "" {
			var uuid pgtype.UUID
			if err := uuid.Scan(viewID); err != nil {
				http.Error(w, "閲覧IDの指定が不正です", http.StatusBadRequest)
				return
			}
			viewID = uuid.String()
		}
		if err := a.reading.Display(r.Context(), n, viewID); err != nil {
			fail(w, err)
			return
		}
	}

	status, err := a.reading.ContentStatus(r.Context(), n)
	if err != nil {
		fail(w, err)
		return
	}
	// Keep upstream error details in storage; the UI only needs to know whether extraction failed.
	if status.Error != "" {
		status.Error = "本文の取得に失敗しました。"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(status); err != nil {
		log.Printf("write content status: %v", err)
	}
}

func opmlCount(raw string) int {
	count, err := strconv.Atoi(raw)
	if err != nil || count < 0 || count > 10000 {
		return 0
	}
	return count
}
