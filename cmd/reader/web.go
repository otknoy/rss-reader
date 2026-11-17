package main

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
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
	mux.HandleFunc("POST /feeds/{id}/remove", a.removeFeed)
	mux.HandleFunc("POST /refresh", a.refresh)
	mux.HandleFunc("POST /articles", a.addArticle)
	mux.HandleFunc("POST /articles/{id}/state", a.state)
	mux.HandleFunc("POST /articles/{id}/fetch-content", a.fetchContent)
	mux.HandleFunc("POST /articles/{id}/delete", a.deleteArticle)
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
		r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
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
	result, err := a.search.Find(r.Context(), articles.Query{Text: q.Get("q"), View: filter, FeedID: feedID, Page: p})
	if err != nil {
		return view{}, err
	}
	q.Del("message")
	q.Del("article")
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
	message := ""
	switch r.URL.Query().Get("message") {
	case "saved":
		message = "保存しました"
	case "updated":
		message = "記事を更新しました"
	case "fetch-error":
		message = "取得できなかったフィードがあります。「購読フィード」の取得状況をご確認ください。"
	case "content-saved":
		message = "本文を取得しました"
	case "content-error":
		message = "本文を取得できませんでした。元記事でご確認ください。"
	case "removed":
		message = "解除しました。取得済みの記事は残しています。"
	}
	return view{Feeds: list, Result: result, Query: r.URL.Query().Get("q"), Filter: filter, FeedID: feedID, ReturnTo: back, ListURL: back, Previous: prev, Next: next, Message: message}, nil
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
	if err := a.feeds.Add(r.Context(), raw); err != nil {
		fail(w, err)
		return
	}
	redirect(w, r, "saved")
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

func (a *application) deleteArticle(w http.ResponseWriter, r *http.Request) {
	n, err := id(r)
	if err == nil {
		err = a.articles.Delete(r.Context(), n)
	}
	if err != nil {
		fail(w, err)
		return
	}
	// Remove only the deleted selection; preserve search, feed and page.
	target, _ := url.Parse(returnTo(r))
	if target.Query().Get("article") == strconv.FormatInt(n, 10) || strings.HasPrefix(target.Path, "/articles/") {
		http.Redirect(w, r, articleURL(target.String(), 0), http.StatusSeeOther)
		return
	}
	redirect(w, r, "")
}

func (a *application) fetchContent(w http.ResponseWriter, r *http.Request) {
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
	content, err := a.fetcher.FetchArticle(r.Context(), item.URL)
	target := articleURL(returnTo(r), n)
	if err != nil {
		log.Printf("fetch article %d: %v", n, err)
		http.Redirect(w, r, withMessage(target, "content-error"), http.StatusSeeOther)
		return
	}
	if err := a.articles.SaveFullContent(r.Context(), n, content); err != nil {
		fail(w, err)
		return
	}
	http.Redirect(w, r, withMessage(target, "content-saved"), http.StatusSeeOther)
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

func withMessage(target, message string) string {
	u, _ := url.Parse(target)
	q := u.Query()
	q.Set("message", message)
	u.RawQuery = q.Encode()
	return u.String()
}
