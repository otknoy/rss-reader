package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"rss-reader/internal/articles"
	"rss-reader/internal/feeds"
	"rss-reader/internal/ingestion"
	"rss-reader/internal/search"
	"rss-reader/migrations"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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
	defer func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close admin database: %v", err)
		}
	}()
	name := fmt.Sprintf("reader_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	var connections []*sql.DB
	defer func() {
		for _, db := range connections {
			if err := db.Close(); err != nil {
				t.Errorf("close database: %v", err)
			}
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
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "42501" {
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
	t.Run("durable TTL schedule", func(t *testing.T) {
		feedID := list[0].ID
		for _, tc := range []struct {
			ttl   int64
			delay time.Duration
		}{{60, time.Hour}, {10, 30 * time.Minute}, {0, 30 * time.Minute}} {
			metadata := feeds.FetchResult{Title: "scheduled", TTLMinutes: tc.ttl, ETag: `"v1"`, LastModified: "Mon, 16 Nov 2026 00:00:00 GMT"}
			if err := fs.Record(ctx, feedID, metadata, 30*time.Minute); err != nil {
				t.Fatal(err)
			}
			// A fresh store / connection sees the persisted due time, as after a restart.
			restarted := feeds.New(connect(os.Getenv("FEEDS_DATABASE_URL")))
			recorded, err := restarted.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			f := recorded[0]
			if f.CheckedAt == nil || f.NextFetchAt == nil || f.LastSuccessAt == nil || f.TTLMinutes != tc.ttl || f.NextFetchAt.Sub(*f.CheckedAt) != tc.delay {
				t.Fatalf("bad schedule: %+v", f)
			}
			if due, err := restarted.ListDue(ctx); err != nil || len(due) != 0 {
				t.Fatalf("fetched too early: %v %v", due, err)
			}
			if err := fs.Add(ctx, f.URL); err != nil {
				t.Fatal(err)
			}
			if due, err := fs.ListDue(ctx); err != nil || len(due) != 0 {
				t.Fatalf("duplicate subscription reset due time: %v %v", due, err)
			}
			metadata.Err = errors.New("temporary fetch failure")
			if err := fs.Record(ctx, feedID, metadata, 30*time.Minute); err != nil {
				t.Fatal(err)
			}
			failed, err := fs.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if failed[0].LastError == "" || !failed[0].LastSuccessAt.Equal(*f.LastSuccessAt) || failed[0].NextFetchAt.Sub(*failed[0].CheckedAt) != tc.delay {
				t.Fatalf("failure lost schedule or success state: %+v", failed[0])
			}
		}
		if _, err := owner.ExecContext(ctx, `UPDATE feeds.subscriptions SET next_fetch_at=now()-interval '1 second' WHERE id=$1`, feedID); err != nil {
			t.Fatal(err)
		}
		if due, err := fs.ListDue(ctx); err != nil || len(due) != 1 {
			t.Fatalf("due feed missing: %v %v", due, err)
		}
		if err := fs.Remove(ctx, feedID); err != nil {
			t.Fatal(err)
		}
		if due, err := fs.ListDue(ctx); err != nil || len(due) != 0 {
			t.Fatalf("inactive feed scheduled: %v %v", due, err)
		}
		if err := fs.Add(ctx, list[0].URL); err != nil {
			t.Fatal(err)
		}
		if due, err := fs.ListDue(ctx); err != nil || len(due) != 1 {
			t.Fatalf("resubscription not scheduled: %v %v", due, err)
		}
	})
	t.Run("cross-process refresh lock", func(t *testing.T) {
		other := feeds.New(connect(os.Getenv("FEEDS_DATABASE_URL")))
		canceled, cancelLock := context.WithCancel(ctx)
		defer cancelLock()
		callbackErr := errors.New("fetch failed")
		err := fs.WithRefreshLock(canceled, func() error {
			called := false
			if err := other.WithRefreshLock(ctx, func() error { called = true; return nil }); !errors.Is(err, feeds.ErrRefreshInProgress) || called {
				t.Fatalf("overlapping refresh allowed: %v", err)
			}
			cancelLock()
			return callbackErr
		})
		if !errors.Is(err, callbackErr) {
			t.Fatalf("callback error lost: %v", err)
		}
		if err := other.WithRefreshLock(ctx, func() error { return nil }); err != nil {
			t.Fatalf("lock leaked after canceled/error run: %v", err)
		}
	})

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
	full := articles.FullContent{Title: "取得した記事", HTML: `<h2>全文</h2><p>全文だけのキーワード</p><img src="/photo.jpg">`, Text: "全文だけのキーワード", URL: "https://example.com/one"}
	if err := as.SaveFullContent(ctx, articleID, full); err != nil {
		t.Fatal(err)
	}
	// RSS refresh must not erase an explicitly fetched full article.
	if err := as.Save(ctx, article); err != nil {
		t.Fatal(err)
	}
	complete, err := as.Get(ctx, articleID)
	if err != nil || complete.FullBodyHTML != full.HTML || complete.FullFetchedAt == nil || complete.BodyText != article.BodyText || !complete.IsRead || !complete.Favorite {
		t.Fatalf("full content or state lost: %+v %v", complete, err)
	}
	if found, err := as.Search(ctx, articles.Query{Text: full.Text}); err != nil || found.Total != 1 {
		t.Fatalf("full article is not searchable: %+v %v", found, err)
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
	app := &application{feeds: fs, articles: as, search: search.New(as), fetcher: ingestion.New(fs, as, ingestion.DefaultInterval)}
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
	if w := request("GET", fmt.Sprintf("/articles/%d", articleID), nil); w.Code != 200 || !strings.Contains(w.Body.String(), full.Text) {
		t.Fatalf("detail HTTP: %d", w.Code)
	}
	selection := fmt.Sprintf("/?article=%d&q=日本語&feed=%d", articleID, article.SourceID)
	if w := request("GET", selection, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `data-reading-id="`+fmt.Sprint(articleID)+`"`) || !strings.Contains(w.Body.String(), `id="list-pane"`) {
		t.Fatalf("workspace selection: %d %s", w.Code, w.Body.String())
	}
	if w := request("GET", "/?article=invalid", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid selection status: %d", w.Code)
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
	if w := request("POST", fmt.Sprintf("/articles/%d/delete", articleID), url.Values{"return_to": {selection}}); w.Code != 303 || strings.Contains(w.Header().Get("Location"), "article=") || !strings.Contains(w.Header().Get("Location"), "feed=") {
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
