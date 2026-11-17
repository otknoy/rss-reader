package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"rss-reader/internal/articles"
	"rss-reader/internal/feeds"
	"rss-reader/internal/ingestion"
	"rss-reader/internal/search"
	"rss-reader/migrations"
)

// Every run gets its own database. No development tables are cleared.
func TestPrototypeIntegration(t *testing.T) {
	adminDSN := os.Getenv("INTEGRATION_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("run via docker compose --profile test run --build --rm test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("reader_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	var connections []*sql.DB
	defer func() {
		for _, db := range connections {
			db.Close()
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(cleanup, `DROP DATABASE `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	connect := func(dsn string) *sql.DB {
		t.Helper()
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + name
		db, err := sql.Open("pgx", u.String())
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, db)
		return db
	}
	owner := connect(adminDSN)
	if err := migrations.Run(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, owner); err != nil {
		t.Fatalf("migration rerun: %v", err)
	}
	fdb := connect(os.Getenv("FEEDS_DATABASE_URL"))
	adb := connect(os.Getenv("ARTICLES_DATABASE_URL"))
	for _, tc := range []struct {
		db    *sql.DB
		table string
	}{{fdb, "articles.entries"}, {adb, "feeds.subscriptions"}} {
		_, err := tc.db.ExecContext(ctx, `SELECT * FROM `+tc.table)
		pgerr, ok := err.(*pgconn.PgError)
		if !ok || pgerr.Code != "42501" {
			t.Fatalf("expected permission denial for %s, got %v", tc.table, err)
		}
	}
	fs, as := feeds.New(fdb), articles.New(adb)
	if err := fs.Add(ctx, "https://example.com/feed.xml"); err != nil {
		t.Fatal(err)
	}
	list, err := fs.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("feeds=%v, err=%v", list, err)
	}
	article := articles.Article{SourceID: list[0].ID, ExternalID: "entry-1", SourceName: "テスト購読", URL: "https://example.com/one", Title: "Goの記事", BodyText: "日本語の全文検索 100% foo_bar"}
	if err := as.Save(ctx, article); err != nil {
		t.Fatal(err)
	}
	result, err := as.Search(ctx, articles.Query{})
	if err != nil || result.Total != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	articleID := result.Items[0].ID
	for _, kind := range []string{"read", "favorite"} {
		if err := as.SetState(ctx, articleID, kind, true); err != nil {
			t.Fatal(err)
		}
	}
	article.Title = "更新された記事"
	if err := as.Save(ctx, article); err != nil {
		t.Fatal(err)
	}
	saved, err := as.Get(ctx, articleID)
	if err != nil || !saved.IsRead || !saved.Favorite {
		t.Fatalf("state lost: %+v, %v", saved, err)
	}
	if err := fs.Remove(ctx, list[0].ID); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"日本語", "100%", "foo_bar"} {
		r, err := as.Search(ctx, articles.Query{Text: q})
		if err != nil || r.Total != 1 {
			t.Fatalf("query %q: %+v %v", q, r, err)
		}
	}
	r, err := as.Search(ctx, articles.Query{Text: "not_found"})
	if err != nil || r.Total != 0 {
		t.Fatalf("unexpected match: %+v %v", r, err)
	}
	app := &application{feeds: fs, articles: as, search: search.New(as), fetcher: ingestion.New(fs, as)}
	request := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, req)
		return w
	}
	if w := request("GET", "/?q=日本語", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "更新された記事") {
		t.Fatalf("search HTTP: %d %s", w.Code, w.Body.String())
	}
	if w := request("GET", fmt.Sprintf("/articles/%d", articleID), nil); w.Code != 200 || !strings.Contains(w.Body.String(), "100%") {
		t.Fatalf("detail HTTP: %d", w.Code)
	}
	if w := request("POST", "/articles", url.Values{"url": {"https://example.com/manual"}, "title": {"手動登録記事"}, "body": {"保存した本文"}}); w.Code != 303 {
		t.Fatalf("manual add HTTP: %d %s", w.Code, w.Body.String())
	}
	if w := request("POST", fmt.Sprintf("/articles/%d/state", articleID), url.Values{"kind": {"read"}, "value": {"false"}}); w.Code != 303 {
		t.Fatalf("state HTTP: %d", w.Code)
	}
	if w := request("GET", "/?view=unread", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "更新された記事") {
		t.Fatalf("unread HTTP: %d", w.Code)
	}
	if w := request("POST", fmt.Sprintf("/articles/%d/delete", articleID), url.Values{}); w.Code != 303 {
		t.Fatalf("delete HTTP: %d", w.Code)
	}
	if err := as.Save(ctx, article); err != nil {
		t.Fatal(err)
	}
	if found, err := as.Search(ctx, articles.Query{Text: "更新された記事"}); err != nil || found.Total != 0 {
		t.Fatalf("deleted article returned after refetch: %+v %v", found, err)
	}
	if w := request("GET", fmt.Sprintf("/articles/%d", articleID), nil); w.Code != http.StatusNotFound {
		t.Fatalf("deleted article status: %d", w.Code)
	}
}
