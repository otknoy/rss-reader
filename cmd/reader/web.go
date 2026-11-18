package main

import (
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
	"rss-reader/internal/search"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed page.html
var pageHTML string

//go:embed reader.js
var readerJS string

var page = template.Must(template.New("page").Funcs(template.FuncMap{
	"richArticle": richArticle,
	"articleURL":  articleURL,
	"date":        func(t time.Time) string { return t.Format("2006.01.02") },
	"excerpt": func(s string) string {
		r := []rune(s)
		if len(r) > 180 {
			return string(r[:180]) + "…"
		}
		return s
	},
}).Parse(pageHTML))

type application struct {
	feeds    *feeds.Store
	articles *articles.Store
	search   *search.Service
	fetcher  *ingestion.Service
}
type view struct {
	ClosedFolders                                             map[int64]bool
	FolderUnread, FeedUnread                                  map[int64]int
	Folders                                                   []feeds.Folder
	Folder, FolderName                                        string
	Scope                                                     string
	NewestFirst                                               bool
	OldestURL, NewestURL                                      string
	Feeds                                                     []feeds.Feed
	Result                                                    articles.Result
	Query, Filter, Message, ReturnTo, ListURL, Previous, Next string
	FeedID                                                    int64
	Detail                                                    *articles.Article
}

func (a *application) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
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
	mux.HandleFunc("POST /feeds/import", a.importOPML)
	mux.HandleFunc("GET /feeds/export.opml", a.exportOPML)
	mux.HandleFunc("POST /feeds/{id}/remove", a.removeFeed)
	mux.HandleFunc("POST /refresh", a.refresh)
	mux.HandleFunc("POST /articles", a.addArticle)
	mux.HandleFunc("POST /articles/{id}/state", a.state)
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
	if errors.Is(err, sql.ErrNoRows) {
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
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\\r\n") {
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

func (a *application) base(r *http.Request) (view, error) {
	list, err := a.feeds.List(r.Context())
	if err != nil {
		return view{}, err
	}
	q := r.URL.Query()
	feedID, _ := strconv.ParseInt(q.Get("feed"), 10, 64)
	if feedID < 0 {
		feedID = 0
	}
	p, _ := strconv.Atoi(q.Get("page"))
	filter := q.Get("view")
	if filter != "unread" && filter != "favorites" {
		filter = ""
	}
	folders, err := a.feeds.Folders(r.Context())
	if err != nil {
		return view{}, err
	}
	closed, err := a.feeds.ClosedFolders(r.Context())
	if err != nil {
		return view{}, err
	}
	folder := q.Get("folder")
	folderName := ""
	var ids []int64
	scope := "/?"
	newestFirst := q.Get("sort") == "newest"
	if newestFirst {
		scope += "sort=newest&"
	} else {
		q.Del("sort")
	}
	if folder != "" {
		ids = []int64{}
		for _, f := range folders {
			if strconv.FormatInt(f.ID, 10) == folder {
				folderName = f.Name
				break
			}
		}
		if folder == "0" {
			folderName = "未分類"
		}
		if folderName == "" {
			return view{}, sql.ErrNoRows
		}
		for _, f := range list {
			if strconv.FormatInt(f.FolderID, 10) == folder {
				ids = append(ids, f.ID)
			}
		}
		scope += "folder=" + folder + "&"
	} else if feedID > 0 {
		scope += "feed=" + strconv.FormatInt(feedID, 10) + "&"
	}
	result, err := a.search.Find(r.Context(), articles.Query{Text: q.Get("q"), View: filter, FeedID: feedID, FeedIDs: ids, Page: p, NewestFirst: newestFirst})
	if err != nil {
		return view{}, err
	}
	unreadByFeed, err := a.articles.UnreadByFeed(r.Context())
	if err != nil {
		return view{}, err
	}
	folderUnread := make(map[int64]int)
	for _, feed := range list {
		folderUnread[feed.FolderID] += unreadByFeed[feed.ID]
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
	prev, next := "", ""
	if result.Page > 1 {
		q.Set("page", strconv.Itoa(result.Page-1))
		prev = "/?" + q.Encode()
	}
	if result.Page < result.Pages {
		q.Set("page", strconv.Itoa(result.Page+1))
		next = "/?" + q.Encode()
	}
	q.Del("page")
	q.Del("sort")
	oldestURL := "/"
	if len(q) > 0 {
		oldestURL += "?" + q.Encode()
	}
	q.Set("sort", "newest")
	newestURL := "/?" + q.Encode()
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
		message = fmt.Sprintf("OPML: %d件登録・%d件重複・%d件スキップ", opmlCount(r.URL.Query().Get("added")), opmlCount(r.URL.Query().Get("existing")), opmlCount(r.URL.Query().Get("skipped")))
	case "removed":
		message = "解除しました。取得済みの記事は残しています。"
	}
	return view{ClosedFolders: closed, FolderUnread: folderUnread, FeedUnread: unreadByFeed, Folders: folders, Folder: folder, FolderName: folderName, Scope: scope, NewestFirst: newestFirst, OldestURL: oldestURL, NewestURL: newestURL, Feeds: list, Result: result, Query: r.URL.Query().Get("q"), Filter: filter, FeedID: feedID, ReturnTo: back, ListURL: back, Previous: prev, Next: next, Message: message}, nil
}

func (a *application) index(w http.ResponseWriter, r *http.Request) {
	v, err := a.base(r)
	if err != nil {
		fail(w, err)
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
	v, err := a.base(r)
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
	feedID, err := a.feeds.AddToFolder(r.Context(), raw, folder)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			http.Error(w, "フォルダーがありません", http.StatusBadRequest)
			return
		}
		fail(w, err)
		return
	}
	message := "saved"
	if err := a.fetcher.RefreshFeed(r.Context(), feedID); err != nil {
		if errors.Is(err, feeds.ErrRefreshInProgress) {
			message = "feed-queued"
		} else {
			log.Printf("initial feed refresh %d: %v", feedID, err)
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
	err := a.articles.Save(r.Context(), articles.Article{URL: raw, Title: strings.TrimSpace(r.FormValue("title")), BodyText: strings.TrimSpace(r.FormValue("body"))})
	if err != nil {
		fail(w, err)
		return
	}
	redirect(w, r, "saved")
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
		item, err := a.articles.Get(r.Context(), n)
		if err != nil {
			fail(w, err)
			return
		}
		if item.FullContentPending {
			if _, err := a.fetcher.FetchQueuedContent(r.Context(), a.articles, item); err != nil {
				log.Printf("fetch opened article %d: %v", n, err)
			}
		}
	}
	status, err := a.articles.FullContentStatus(r.Context(), n)
	if err != nil {
		fail(w, err)
		return
	}
	// Keep upstream error details in storage; the UI only needs to know whether a retry is pending.
	if status.Error != "" {
		status.Error = "本文の自動取得に失敗しました。後で再試行します。"
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
