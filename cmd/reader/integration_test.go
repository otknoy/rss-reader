package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"rss-reader/internal/articles"
	articlepostgres "rss-reader/internal/articles/postgres"
	"rss-reader/internal/feeds"
	feedpostgres "rss-reader/internal/feeds/postgres"
	"rss-reader/internal/ingestion"
	"rss-reader/internal/ingestion/httpfetch"
	"rss-reader/migrations"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Every run gets its own database. No development tables are cleared.
func setupIntegrationDB(t *testing.T) (ctx context.Context, owner, fdb, adb *sql.DB, connect func(string) *sql.DB) {
	t.Helper()
	adminDSN := os.Getenv("INTEGRATION_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("run via docker compose --profile test run --build --rm test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close admin database: %v", err)
		}
	})
	name := fmt.Sprintf("reader_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	var connections []*sql.DB
	t.Cleanup(func() {
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
	})
	connect = func(dsn string) *sql.DB {
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
	owner = connect(adminDSN)
	if err := migrations.Run(ctx, owner); err != nil {
		t.Fatal(err)
	}
	fdb = connect(os.Getenv("FEEDS_DATABASE_URL"))
	adb = connect(os.Getenv("ARTICLES_DATABASE_URL"))
	return ctx, owner, fdb, adb, connect
}

func TestDatabasePermissionsIntegration(t *testing.T) {
	ctx, owner, fdb, adb, _ := setupIntegrationDB(t)
	if err := migrations.Run(ctx, owner); err != nil {
		t.Fatalf("migration rerun: %v", err)
	}
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
}

func TestOPMLExportDuringFolderDeletionIntegration(t *testing.T) {
	ctx, owner, fdb, _, _ := setupIntegrationDB(t)
	store := feedpostgres.New(fdb)
	const folderName = "Export snapshot"
	const feedURL = "https://example.org/snapshot.xml"
	if err := store.SaveFolder(ctx, 0, folderName); err != nil {
		t.Fatal(err)
	}
	folders, err := store.Folders(ctx)
	if err != nil || len(folders) != 1 {
		t.Fatalf("load test folder: %v, %v", folders, err)
	}
	folderID := folders[0].ID
	if _, err := store.AddToFolder(ctx, feedURL, &folderID); err != nil {
		t.Fatal(err)
	}

	tx, err := owner.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("release folder lock: %v", err)
		}
	}()

	// Block only the folder query, allowing the export to read subscriptions first.
	if _, err := tx.ExecContext(ctx, `LOCK TABLE feeds.folders IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var blocker int
	if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	exportCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- feeds.ExportOPML(exportCtx, store, &output) }()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err := owner.QueryRowContext(exportCtx, `SELECT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE relation = 'feeds.folders'::regclass AND database = (
				SELECT oid FROM pg_database WHERE datname = current_database()
			) AND NOT granted AND $1 = ANY(pg_blocking_pids(pid))
		)`, blocker).Scan(&blocked)
		if err != nil {
			t.Fatalf("wait for export to read folders: %v", err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("export finished before waiting for the folder lock: %v", err)
		case <-exportCtx.Done():
			t.Fatalf("export did not wait for the folder lock: %v", exportCtx.Err())
		case <-ticker.C:
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM feeds.folders WHERE id=$1`, folderID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-exportCtx.Done():
		t.Fatalf("wait for export: %v", exportCtx.Err())
	}

	type outline struct {
		Name     string    `xml:"text,attr"`
		URL      string    `xml:"xmlUrl,attr"`
		Children []outline `xml:"outline"`
	}
	var document struct {
		Outlines []outline `xml:"body>outline"`
	}
	if err := xml.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Outlines) != 1 ||
		document.Outlines[0].Name != folderName ||
		len(document.Outlines[0].Children) != 1 ||
		document.Outlines[0].Children[0].URL != feedURL {
		t.Fatalf("export should contain the original folder and exactly one subscription: %s", &output)
	}
}

func TestPrototypeIntegration(t *testing.T) {
	ctx, owner, fdb, adb, connect := setupIntegrationDB(t)
	fs, as := feedpostgres.New(fdb), articlepostgres.New(adb)
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
			metadata := feeds.FetchResult{
				Title:        "scheduled",
				TTLMinutes:   tc.ttl,
				ETag:         `"v1"`,
				LastModified: "Mon, 16 Nov 2026 00:00:00 GMT",
			}
			if err := fs.Record(ctx, feedID, metadata, 30*time.Minute); err != nil {
				t.Fatal(err)
			}
			// A fresh store / connection sees the persisted due time, as after a restart.
			restarted := feedpostgres.New(connect(os.Getenv("FEEDS_DATABASE_URL")))
			recorded, err := restarted.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			f := recorded[0]
			if f.CheckedAt == nil ||
				f.NextFetchAt == nil ||
				f.LastSuccessAt == nil ||
				f.TTLMinutes != tc.ttl ||
				f.NextFetchAt.Sub(*f.CheckedAt) != tc.delay {
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
			if due, err := fs.ListDueFeed(ctx, feedID); err != nil || len(due) != 0 {
				t.Fatalf("duplicate subscription is eligible for initial fetch: %v %v", due, err)
			}
			metadata.Err = errors.New("temporary fetch failure")
			if err := fs.Record(ctx, feedID, metadata, 30*time.Minute); err != nil {
				t.Fatal(err)
			}
			failed, err := fs.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if failed[0].LastError == "" ||
				!failed[0].LastSuccessAt.Equal(*f.LastSuccessAt) ||
				failed[0].NextFetchAt.Sub(*failed[0].CheckedAt) != tc.delay {
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
		if due, err := fs.ListDueFeed(ctx, feedID); err != nil || len(due) != 0 {
			t.Fatalf("inactive feed is eligible for initial fetch: %v %v", due, err)
		}
		if err := fs.Add(ctx, list[0].URL); err != nil {
			t.Fatal(err)
		}
		if due, err := fs.ListDue(ctx); err != nil || len(due) != 1 {
			t.Fatalf("resubscription not scheduled: %v %v", due, err)
		}
		if due, err := fs.ListDueFeed(ctx, feedID); err != nil || len(due) != 1 || due[0].ID != feedID {
			t.Fatalf("resubscribed feed missing from initial fetch: %v %v", due, err)
		}
		if due, err := fs.ListDueFeed(ctx, 0); err != nil || len(due) != 0 {
			t.Fatalf("unknown ID matched a feed: %v %v", due, err)
		}
	})
	t.Run("cross-process refresh lock", func(t *testing.T) {
		other := feedpostgres.New(connect(os.Getenv("FEEDS_DATABASE_URL")))
		canceled, cancelLock := context.WithCancel(ctx)
		defer cancelLock()
		callbackErr := errors.New("fetch failed")
		err := fs.WithRefreshLock(canceled, func() error {
			called := false
			if err := other.WithRefreshLock(ctx, func() error { called = true; return nil }); !errors.Is(err, feeds.ErrRefreshInProgress) ||
				called {
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

	article := articles.Article{
		SourceID:   list[0].ID,
		ExternalID: "entry-1",
		SourceName: "テスト購読",
		URL:        "https://example.com/one",
		Title:      "Goの記事",
		BodyText:   "日本語の全文検索 100% foo_bar",
	}
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
	full := articles.FullContent{
		Title: "取得した記事",
		HTML:  `<h2>全文</h2><p>全文だけのキーワード</p><img src="/photo.jpg">`,
		Text:  "全文だけのキーワード",
		URL:   "https://example.com/one",
	}
	if err := as.SaveFullContent(ctx, articleID, full); err != nil {
		t.Fatal(err)
	}
	// This feed has no publication time. Re-saving it should keep the same row version.
	var before, afterSave string
	const rowVersionQuery = `SELECT ctid::text FROM articles.entries WHERE id=$1`
	if err := owner.QueryRowContext(ctx, rowVersionQuery, articleID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := as.Save(ctx, article); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRowContext(ctx, rowVersionQuery, articleID).Scan(&afterSave); err != nil {
		t.Fatal(err)
	}
	if before != afterSave {
		t.Fatalf("unchanged article was rewritten: %s -> %s", before, afterSave)
	}

	article.BodyText += " 訂正"
	if err := as.Save(ctx, article); err != nil {
		t.Fatal(err)
	}
	complete, err := as.Get(ctx, articleID)
	if err != nil ||
		complete.FullBodyHTML != full.HTML ||
		complete.FullFetchedAt == nil ||
		complete.BodyText != article.BodyText ||
		!complete.IsRead ||
		!complete.Favorite {
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
	client := httpfetch.New()
	app := newApplication(fs, as, ingestion.New(fs, as, client, ingestion.DefaultInterval), client)
	request := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, req)
		return w
	}
	if w := request("GET", "/?q=日本語&view=all", nil); w.Code != 200 ||
		!strings.Contains(w.Body.String(), "更新された記事") {
		t.Fatalf("search HTTP: %d %s", w.Code, w.Body.String())
	}
	if w := request("GET", fmt.Sprintf("/articles/%d", articleID), nil); w.Code != 200 ||
		!strings.Contains(w.Body.String(), full.Text) {
		t.Fatalf("detail HTTP: %d", w.Code)
	}
	t.Run("read-only database inspection", func(t *testing.T) {
		for _, table := range []string{
			"subscriptions",
			"folders",
			"closed_folders",
			"entries",
			"full_contents",
			"article_views",
		} {
			if w := request("GET", "/admin/database?table="+table, nil); w.Code != http.StatusOK {
				t.Fatalf("inspect %s: %d", table, w.Code)
			}
		}
		list := request("GET", "/admin/database?table=entries", nil)
		if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "<table") ||
			!strings.Contains(list.Body.String(), "<code>is_read</code>") {
			t.Fatalf("inspect article table: %d", list.Code)
		}
		w := request("GET", fmt.Sprintf("/admin/database?table=entries&id=%d", articleID), nil)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<code>body_text</code>") ||
			strings.Contains(w.Body.String(), "full_body_text") {
			t.Fatalf("inspect RSS article record: %d", w.Code)
		}
		fullRecord := request("GET", fmt.Sprintf("/admin/database?table=full_contents&id=%d", articleID), nil)
		if fullRecord.Code != http.StatusOK || !strings.Contains(fullRecord.Body.String(), full.Text) ||
			!strings.Contains(fullRecord.Body.String(), "<code>fetched_at</code>") {
			t.Fatalf("inspect full content record: %d", fullRecord.Code)
		}
		if w := request("GET", "/admin/database?table=unknown", nil); w.Code != http.StatusNotFound {
			t.Fatalf("unknown table status: %d", w.Code)
		}
		if w := request("POST", "/admin/database", nil); w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("database inspection accepted a write method: %d", w.Code)
		}
	})
	selection := fmt.Sprintf("/?article=%d&q=日本語&feed=%d", articleID, article.SourceID)
	if w := request("GET", selection, nil); w.Code != 200 ||
		!strings.Contains(w.Body.String(), `data-reading-id="`+fmt.Sprint(articleID)+`"`) ||
		!strings.Contains(w.Body.String(), `id="list-pane"`) {
		t.Fatalf("workspace selection: %d %s", w.Code, w.Body.String())
	}
	if w := request("GET", "/?article=invalid", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid selection status: %d", w.Code)
	}
	if w := request("POST", "/articles", url.Values{
		"url":   {"https://example.com/manual"},
		"title": {"手動登録記事"},
		"body":  {"保存した本文"},
	}); w.Code != 303 {
		t.Fatalf("manual add HTTP: %d %s", w.Code, w.Body.String())
	}
	if w := request("GET", "/?view=read", nil); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), "更新された記事") || strings.Contains(w.Body.String(), "手動登録記事") {
		t.Fatalf("read filter did not select only read articles: %d", w.Code)
	}
	if w := request("GET", "/", nil); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `href="/?view=all" data-workspace-link data-unread-filter aria-current="true"`) ||
		strings.Contains(w.Body.String(), "更新された記事") || !strings.Contains(w.Body.String(), "手動登録記事") {
		t.Fatalf("default view did not select only unread articles: %d", w.Code)
	}
	if w := request("GET", "/?view=all", nil); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), "更新された記事") || !strings.Contains(w.Body.String(), "手動登録記事") {
		t.Fatalf("all view did not include both read and unread articles: %d", w.Code)
	}
	if w := request("POST", fmt.Sprintf("/articles/%d/state", articleID), url.Values{"kind": {"read"}, "value": {"false"}}); w.Code != 303 {
		t.Fatalf("state HTTP: %d", w.Code)
	}
	if w := request("GET", "/?view=unread", nil); w.Code != 200 ||
		!strings.Contains(w.Body.String(), "更新された記事") {
		t.Fatalf("unread HTTP: %d", w.Code)
	}
	if w := request("GET", "/?view=unread-favorites", nil); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), "更新された記事") || strings.Contains(w.Body.String(), "手動登録記事") {
		t.Fatalf("combined unread and favorite filters: %d", w.Code)
	}
	t.Run("open article returns only the reader pane", func(t *testing.T) {
		path := fmt.Sprintf("/articles/%d/open", articleID)
		for _, changed := range []bool{true, false} {
			w := request("POST", path, url.Values{"return_to": {selection}})
			var opened struct {
				HTML, SourceID string
				Changed        bool
			}
			if err := json.Unmarshal(w.Body.Bytes(), &opened); err != nil {
				t.Fatalf("open response: %d %v", w.Code, err)
			}
			if w.Code != http.StatusOK || opened.Changed != changed || opened.SourceID != fmt.Sprint(article.SourceID) ||
				!strings.Contains(opened.HTML, full.Text) || !strings.Contains(opened.HTML, "未読に戻す") ||
				strings.Contains(opened.HTML, `id="list-pane"`) || !strings.Contains(opened.HTML, `q=`) {
				t.Fatalf("unexpected partial open response: %d, changed=%v", w.Code, opened.Changed)
			}
		}
	})
	if w := request("POST", fmt.Sprintf("/articles/%d/delete", articleID), url.Values{"return_to": {selection}}); w.Code != http.StatusNotFound {
		t.Fatalf("article deletion is still exposed over HTTP: %d", w.Code)
	}
	if w := request("GET", selection, nil); w.Code != http.StatusOK {
		t.Fatalf("rejected deletion changed article: %d", w.Code)
	}
	// Preserve support for historical deleted rows without exposing deletion in the Web UI.
	if err := as.Delete(ctx, articleID); err != nil {
		t.Fatal(err)
	}

	if err := as.Save(ctx, article); err != nil {
		t.Fatal(err)
	}
	if found, err := as.Search(ctx, articles.Query{Text: "更新された記事"}); err != nil ||
		found.Total != 0 {
		t.Fatalf("deleted article returned after refetch: %+v %v", found, err)
	}
	if w := request("GET", fmt.Sprintf("/articles/%d", articleID), nil); w.Code != http.StatusNotFound {
		t.Fatalf("deleted article status: %d", w.Code)
	}
	t.Run("full content on article display", func(t *testing.T) {
		manual, err := as.Search(ctx, articles.Query{Text: "手動登録記事"})
		if err != nil || manual.Total != 1 {
			t.Fatalf("manual article missing: %+v %v", manual, err)
		}
		manualItem := manual.Items[0]
		if err := as.Delete(ctx, manualItem.ID); err != nil {
			t.Fatal(err)
		}
		if err := as.Save(ctx, articles.Article{URL: manualItem.URL, Title: manualItem.Title, BodyText: "保存した本文"}); err != nil {
			t.Fatal(err)
		}
		if _, err := as.Get(ctx, manualItem.ID); err != nil {
			t.Fatalf("re-registering an unchanged manual article should restore it: %v", err)
		}
		if status, err := as.FullContentStatus(ctx, manualItem.ID); err != nil || status.Pending {
			t.Fatalf("manual registration unexpectedly queued: %+v %v", status, err)
		}
		if err := as.SaveFullContent(ctx, manualItem.ID, articles.FullContent{
			HTML: "<p>手動登録記事の本文</p>",
			Text: "手動登録記事の本文",
			URL:  manualItem.URL,
		}); err != nil {
			t.Fatal(err)
		}
		manualContentURL := fmt.Sprintf("/articles/%d/content-status", manualItem.ID)
		if w := request("POST", manualContentURL, nil); w.Code != http.StatusOK ||
			!strings.Contains(w.Body.String(), `"pending":false`) {
			t.Fatalf("already fetched article was queued on first display: %d %s", w.Code, w.Body.String())
		}
		var manualOpenedAt *time.Time
		if err := owner.QueryRowContext(ctx, `SELECT first_opened_at FROM articles.entries WHERE id=$1`, manualItem.ID).Scan(&manualOpenedAt); err != nil ||
			manualOpenedAt == nil {
			t.Fatalf("display of already fetched article was not recorded: %v %v", manualOpenedAt, err)
		}
		postView := func(viewID string) int {
			req := httptest.NewRequest(http.MethodPost, manualContentURL, nil)
			req.Header.Set("X-Article-View-ID", viewID)
			w := httptest.NewRecorder()
			app.routes().ServeHTTP(w, req)
			return w.Code
		}
		if status := postView("invalid"); status != http.StatusBadRequest {
			t.Fatalf("invalid viewing ID: status %d", status)
		}
		viewID := "d67d845a-0fb5-46ee-9097-d440d014efdb"
		for range 2 {
			if status := postView(viewID); status != http.StatusOK {
				t.Fatalf("viewing event: status %d", status)
			}
		}
		var count int
		var last, event time.Time
		if err := owner.QueryRowContext(ctx, `SELECT count(*), e.last_opened_at, max(v.viewed_at)
			FROM articles.entries e JOIN articles.article_views v ON v.article_id=e.id
			WHERE e.id=$1 GROUP BY e.id`, manualItem.ID).Scan(&count, &last, &event); err != nil {
			t.Fatal(err)
		}
		if count != 2 || !last.Equal(event) {
			t.Fatalf("expected two distinct displays and matching latest times: %d, %v, %v", count, last, event)
		}
		viewList := request("GET", "/admin/database?table=article_views", nil)
		if viewList.Code != http.StatusOK || !strings.Contains(viewList.Body.String(), "<code>article_id</code>") ||
			!strings.Contains(viewList.Body.String(), "<code>viewed_at</code>") ||
			strings.Count(viewList.Body.String(), viewID) != 1 {
			t.Fatalf("inspect viewing events: status %d", viewList.Code)
		}
		queued := articles.Article{
			SourceID:   999,
			ExternalID: "queue-one",
			URL:        "https://example.com/queue-one",
			Title:      "取得待ちのテスト",
			BodyText:   "RSSの要約",
		}
		if err := as.Save(ctx, queued); err != nil {
			t.Fatal(err)
		}
		matching, err := as.Search(ctx, articles.Query{Text: queued.Title})
		if err != nil || matching.Total != 1 {
			t.Fatalf("queued article: %v %v", matching, err)
		}
		queuedID := matching.Items[0].ID
		freshStore := articlepostgres.New(connect(os.Getenv("ARTICLES_DATABASE_URL")))
		if _, err := as.MarkRead(ctx, []int64{queuedID}); err != nil {
			t.Fatal(err)
		}
		if w := request("GET", fmt.Sprintf("/articles/%d", queuedID), nil); w.Code != http.StatusOK ||
			!strings.Contains(w.Body.String(), "RSSの要約") {
			t.Fatalf("direct article did not show its RSS summary: %d %s", w.Code, w.Body.String())
		}
		var openedAt *time.Time
		if err := owner.QueryRowContext(ctx, `SELECT first_opened_at FROM articles.entries WHERE id=$1`, queuedID).Scan(&openedAt); err != nil ||
			openedAt != nil {
			t.Fatalf("read or GET changed first display: %v %v", openedAt, err)
		}
		contentURL := fmt.Sprintf("/articles/%d/content-status", queuedID)
		if w := request("GET", contentURL, nil); w.Code != http.StatusOK ||
			!strings.Contains(w.Body.String(), `"pending":false`) {
			t.Fatalf("undisplayed article was queued: %d %s", w.Code, w.Body.String())
		}
		if err := as.RecordDisplay(ctx, queuedID, ""); err != nil {
			t.Fatal(err)
		}
		if err := owner.QueryRowContext(ctx, `SELECT first_opened_at FROM articles.entries WHERE id=$1`, queuedID).Scan(&openedAt); err != nil ||
			openedAt == nil {
			t.Fatalf("first display was not recorded: %v %v", openedAt, err)
		}

		if err := as.Save(ctx, queued); err != nil {
			t.Fatal(err)
		}
		if url, err := as.ClaimFullContent(ctx, queuedID, false); err != nil || url != queued.URL {
			t.Fatalf("first claim: %q %v", url, err)
		}
		if url, err := freshStore.ClaimFullContent(ctx, queuedID, false); err != nil || url != "" {
			t.Fatalf("overlapping claim: %q %v", url, err)
		}
		if err := as.FailFullContent(ctx, queuedID, errors.New("temporary failure")); err != nil {
			t.Fatal(err)
		}
		if w := request("POST", contentURL, nil); w.Code != http.StatusOK ||
			!strings.Contains(w.Body.String(), `"pending":false`) ||
			!strings.Contains(w.Body.String(), "本文の取得に失敗しました") {
			t.Fatalf("reopening after failure: %d %s", w.Code, w.Body.String())
		}
		if url, err := freshStore.ClaimFullContent(ctx, queuedID, false); err != nil || url != "" {
			t.Fatalf("failed article automatically reclaimed: %q %v", url, err)
		}
		if w := request("GET", fmt.Sprintf("/articles/%d", queuedID), nil); w.Code != http.StatusOK ||
			!strings.Contains(w.Body.String(), "本文を再取得") || !strings.Contains(w.Body.String(), queued.BodyText) {
			t.Fatalf("missing retry or RSS summary: %d", w.Code)
		}
		if url, err := freshStore.ClaimFullContent(ctx, queuedID, true); err != nil || url != queued.URL {
			t.Fatalf("manual retry claim: %q %v", url, err)
		}
		if err := as.SaveFullContent(ctx, queuedID, articles.FullContent{
			HTML: "<p>取得済み本文</p>",
			Text: "取得済み本文",
			URL:  queued.URL,
		}); err != nil {
			t.Fatal(err)
		}
		if err := as.Save(ctx, queued); err != nil {
			t.Fatal(err)
		}
		status, err := as.FullContentStatus(ctx, queuedID)
		if err != nil || status.Pending || status.Error != "" {
			t.Fatalf("completed article was requeued: %+v %v", status, err)
		}
		if w := request("GET", fmt.Sprintf("/articles/%d/content-status", queuedID), nil); w.Code != http.StatusOK ||
			!strings.Contains(w.Body.String(), `"pending":false`) {
			t.Fatalf("content status endpoint: %d %s", w.Code, w.Body.String())
		}
		if w := request("POST", contentURL, nil); w.Code != http.StatusOK ||
			!strings.Contains(w.Body.String(), `"pending":false`) {
			t.Fatalf("display of fetched article requeued it: %d %s", w.Code, w.Body.String())
		}
		var reopenedAt time.Time
		if err := owner.QueryRowContext(ctx, `SELECT first_opened_at FROM articles.entries WHERE id=$1`, queuedID).Scan(&reopenedAt); err != nil ||
			!reopenedAt.Equal(*openedAt) {
			t.Fatalf("reopening changed first display: %v %v", reopenedAt, err)
		}

	})
	t.Run("OPML import export and atomic merge", func(t *testing.T) {
		existingURL := "https://opml.example.org/existing?a=1&b=2"
		archivedURL := "https://opml.example.org/archived"
		newURL := "https://opml.example.org/new"
		for _, u := range []string{existingURL, archivedURL} {
			if err := fs.Add(ctx, u); err != nil {
				t.Fatal(err)
			}
		}
		find := func(list []feeds.Subscription, url string) feeds.Subscription {
			t.Helper()
			for _, f := range list {
				if f.URL == url {
					return f
				}
			}
			t.Fatalf("feed not found: %s", url)
			return feeds.Subscription{}
		}
		initial, err := fs.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		existing, archived := find(initial, existingURL), find(initial, archivedURL)
		if err := fs.Record(
			ctx,
			existing.ID,
			feeds.FetchResult{
				Title:      "既存の名前",
				TTLMinutes: 60,
				ETag:       `"stable"`,
			},
			30*time.Minute,
		); err != nil {
			t.Fatal(err)
		}
		if err := fs.Remove(ctx, archived.ID); err != nil {
			t.Fatal(err)
		}
		before, err := fs.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		saved := find(before, existingURL)
		input := `<opml version="2.0"><body><outline text="フォルダー"><outline title="新しい名前には変更しない" xmlUrl="https://opml.example.org/existing?a=1&amp;b=2"/><outline title="日本語 &amp; 新着" xmlUrl="https://opml.example.org/new"/><outline xmlUrl="https://opml.example.org/archived"/><outline xmlUrl="https://opml.example.org/existing?a=1&amp;b=2"/><outline xmlUrl="javascript:alert(1)"/></outline></body></opml>`
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, opmlUpload(t, []byte(input), "/?q=Go&view=unread"))
		location, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		q := location.Query()
		if w.Code != http.StatusSeeOther ||
			q.Get("added") != "2" ||
			q.Get("existing") != "2" ||
			q.Get("skipped") != "1" ||
			q.Get("q") != "Go" ||
			q.Get("view") != "unread" {
			t.Fatalf("import response: %d %s %s", w.Code, location, w.Body.String())
		}
		after, err := fs.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before)+2 {
			t.Fatalf("subscription count: %d -> %d", len(before), len(after))
		}
		unchanged := find(after, existingURL)
		if unchanged.Title != saved.Title ||
			unchanged.ETag != saved.ETag ||
			unchanged.NextFetchAt == nil ||
			!unchanged.NextFetchAt.Equal(*saved.NextFetchAt) {
			t.Fatalf("existing subscription changed: %+v", unchanged)
		}
		restored := find(after, archivedURL)
		if restored.ID != archived.ID || restored.NextFetchAt != nil {
			t.Fatalf("reactivation lost identity or fetch schedule: %+v", restored)
		}
		if find(after, newURL).Title != "日本語 & 新着" {
			t.Fatal("OPML title was lost")
		}
		// A syntactically broken file must not import an otherwise valid first entry.
		if _, err := feeds.ImportOPML(ctx, fs, strings.NewReader(`<opml><body><outline xmlUrl="https://opml.example.org/partial"/></body>`)); !errors.Is(err, feeds.ErrInvalidOPML) {
			t.Fatalf("malformed XML accepted: %v", err)
		}
		current, err := fs.List(ctx)
		if err != nil || len(current) != len(after) {
			t.Fatalf("malformed import had partial effects: %v %v", current, err)
		}
		if err := fs.SaveFolder(ctx, 0, "空のフォルダー"); err != nil {
			t.Fatal(err)
		}
		folders, err := fs.Folders(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var emptyID int64
		for _, folder := range folders {
			if folder.Name == "空のフォルダー" {
				emptyID = folder.ID
			}
		}
		if emptyID == 0 {
			t.Fatal("folder not created")
		}
		emptyFolderSummary := regexp.MustCompile(`<span>空のフォルダー</span></a>\s*</summary>`)
		if w := request("GET", fmt.Sprintf("/?folder=%d", emptyID), nil); w.Code != http.StatusOK ||
			strings.Contains(w.Body.String(), "data-article-id=") ||
			!emptyFolderSummary.MatchString(w.Body.String()) {
			t.Fatalf("empty folder response: %d", w.Code)
		}
		for _, folderID := range []int64{0, emptyID} {
			path := fmt.Sprintf("/folders/open/%d", folderID)
			for _, open := range []bool{false, false, true} {
				if w := request("POST", path, url.Values{"open": {fmt.Sprint(open)}}); w.Code != http.StatusNoContent {
					t.Fatalf("save folder state: %d %s", w.Code, w.Body.String())
				}
				page := request("GET", "/", nil)
				marker := fmt.Sprintf(`<details open data-sidebar-folder="%d"`, folderID)
				if page.Code != http.StatusOK || strings.Contains(page.Body.String(), marker) != open {
					t.Fatalf("server-rendered folder %d open=%v", folderID, open)
				}
			}
		}

		if err := fs.Move(ctx, existing.ID, emptyID); err != nil {
			t.Fatal(err)
		}
		if err := as.Save(ctx, articles.Article{
			SourceID: existing.ID,
			URL:      "https://opml.example.org/folder-unread",
		}); err != nil {
			t.Fatal(err)
		}
		page := request("GET", "/?q=no-matching-article", nil)
		if page.Code != http.StatusOK ||
			!strings.Contains(page.Body.String(), `<span>空のフォルダー</span><span class="unread-badge"><span class="visually-hidden">未読 </span><span data-unread-count>1`) {
			t.Fatalf("folder unread count missing from filtered page: %d", page.Code)
		}
		if !strings.Contains(page.Body.String(), fmt.Sprintf(`<span>%s</span><span class="unread-badge"><span class="visually-hidden">未読 </span><span data-unread-count>1`, saved.Title)) {
			t.Fatal("feed unread count missing from filtered page")
		}
		first, second := fmt.Sprint(emptyID), fmt.Sprint(find(after, newURL).FolderID)
		currentFolders, err := fs.Folders(ctx)
		if err != nil || len(currentFolders) != 2 {
			t.Fatalf("load current folder order: %v %v", currentFolders, err)
		}
		previous := []string{fmt.Sprint(currentFolders[0].ID), fmt.Sprint(currentFolders[1].ID)}
		if w := request("POST", "/folders/order", url.Values{
			"folder":          {first, first},
			"previous_folder": previous,
		}); w.Code != http.StatusBadRequest {
			t.Fatalf("duplicate folder order: %d %s", w.Code, w.Body.String())
		}
		for _, order := range [][]string{{first, second}, {second, first}} {
			if w := request("POST", "/folders/order", url.Values{"folder": order, "previous_folder": previous}); w.Code != http.StatusSeeOther {
				t.Fatalf("save folder order: %d %s", w.Code, w.Body.String())
			}
			stale := []string{order[1], order[0]}
			if w := request("POST", "/folders/order", url.Values{"folder": stale, "previous_folder": stale}); w.Code != http.StatusConflict {
				t.Fatalf("stale folder order: %d %s", w.Code, w.Body.String())
			}
			previous = order
			page := request("GET", "/", nil).Body.String()
			firstIndex := strings.Index(page, `data-sidebar-folder="`+order[0]+`"`)
			secondIndex := strings.Index(page, `data-sidebar-folder="`+order[1]+`"`)
			if firstIndex < 0 || secondIndex <= firstIndex {
				t.Fatal("new page did not use the saved folder order")
			}
		}
		if err := fs.SaveFolder(ctx, emptyID, "改名済み"); err != nil {
			t.Fatal(err)
		}
		if err := fs.DeleteFolder(ctx, emptyID); err != nil {
			t.Fatal(err)
		}
		after, err = fs.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if find(after, existingURL).FolderID != 0 || find(after, newURL).FolderID == 0 {
			t.Fatal("folder removal or OPML classification failed")
		}

		var output, again bytes.Buffer
		if err := feeds.ExportOPML(ctx, fs, &output); err != nil {
			t.Fatal(err)
		}
		var document struct {
			Version string `xml:"version,attr"`
			Body    struct {
				Outlines []struct {
					URL      string `xml:"xmlUrl,attr"`
					Children []struct {
						URL string `xml:"xmlUrl,attr"`
					} `xml:"outline"`
				} `xml:"outline"`
			} `xml:"body"`
		}
		if err := xml.Unmarshal(output.Bytes(), &document); err != nil {
			t.Fatal(err)
		}
		feedCount := 0
		for _, outline := range document.Body.Outlines {
			if outline.URL != "" {
				feedCount++
			}
			feedCount += len(outline.Children)
		}
		if document.Version != "2.0" ||
			feedCount != len(after) ||
			!strings.Contains(output.String(), "日本語 &amp; 新着") {
			t.Fatalf("invalid export: %s", output.String())
		}
		result, err := feeds.ImportOPML(ctx, fs, bytes.NewReader(output.Bytes()))
		if err != nil || result.Added != 0 || result.Existing != len(after) || result.Skipped != 0 {
			t.Fatalf("round trip: %+v %v", result, err)
		}
		if err := feeds.ExportOPML(ctx, fs, &again); err != nil {
			t.Fatal(err)
		}
		if output.String() != again.String() {
			t.Fatal("export is not deterministic")
		}
		exported := request("GET", "/feeds/export.opml", nil)
		if exported.Code != http.StatusOK ||
			!strings.Contains(exported.Header().Get("Content-Disposition"), "subscriptions.opml") ||
			exported.Body.String() != output.String() {
			t.Fatalf("download response: %d %v", exported.Code, exported.Header())
		}
		// Simulate a DB failure after the first insert to verify transaction rollback.
		if _, err := owner.ExecContext(ctx, `ALTER TABLE feeds.subscriptions ADD CONSTRAINT opml_test_reject CHECK (url <> 'https://opml.example.org/reject')`); err != nil {
			t.Fatal(err)
		}
		failed := `<opml><body><outline xmlUrl="https://opml.example.org/rollback"/><outline xmlUrl="https://opml.example.org/reject"/></body></opml>`
		if _, err := feeds.ImportOPML(ctx, fs, strings.NewReader(failed)); err == nil {
			t.Fatal("expected database import failure")
		}
		current, err = fs.List(ctx)
		if err != nil || len(current) != len(after) {
			t.Fatalf("failed transaction had partial effects: %v %v", current, err)
		}
	})
	t.Run("viewing history", func(t *testing.T) {
		for i := 1; i <= 22; i++ {
			title := fmt.Sprintf("history-fixture-%02d", i)
			if err := as.Save(ctx, articles.Article{Title: title, URL: "https://example.org/" + title}); err != nil {
				t.Fatal(err)
			}
		}
		// Equal timestamps exercise the ID tie-breaker at the page boundary.
		if _, err := owner.ExecContext(ctx, `UPDATE articles.entries
			SET last_opened_at = '2025-02-01 00:00:00+00'
			WHERE title LIKE 'history-fixture-%' AND title <> 'history-fixture-22'`); err != nil {
			t.Fatal(err)
		}
		var boundaryID, unopenedID int64
		for title, id := range map[string]*int64{
			"history-fixture-02": &boundaryID,
			"history-fixture-22": &unopenedID,
		} {
			if err := owner.QueryRowContext(ctx, "SELECT id FROM articles.entries WHERE title=$1", title).Scan(id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := as.MarkRead(ctx, []int64{unopenedID}); err != nil {
			t.Fatal(err)
		}
		historyURL := "/?q=history-fixture&view=history"
		first := request("GET", historyURL, nil)
		firstBody := first.Body.String()
		next := regexp.MustCompile(`data-next-articles href="([^"]+)"`).FindStringSubmatch(firstBody)
		if first.Code != http.StatusOK || len(next) != 2 ||
			!strings.Contains(firstBody, ">history-fixture-02</a>") ||
			strings.Contains(firstBody, ">history-fixture-01</a>") ||
			strings.Contains(firstBody, ">history-fixture-22</a>") {
			t.Fatalf("unexpected first history page: status %d", first.Code)
		}
		if err := as.RecordDisplay(ctx, boundaryID, ""); err != nil {
			t.Fatal(err)
		}
		second := request("GET", strings.ReplaceAll(next[1], "&amp;", "&"), nil)
		if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), ">history-fixture-01</a>") ||
			strings.Contains(second.Body.String(), ">history-fixture-02</a>") {
			t.Fatalf("unexpected second history page after reopening the boundary: status %d", second.Code)
		}
		refreshed := request("GET", historyURL, nil)
		rows := regexp.MustCompile(`<h2><a [^>]+>(history-fixture-[0-9]+)</a></h2>`).FindAllStringSubmatch(refreshed.Body.String(), -1)
		if refreshed.Code != http.StatusOK || len(rows) != 20 || rows[0][1] != "history-fixture-02" {
			t.Fatalf("unexpected history order after reopening: status %d, rows %v", refreshed.Code, rows)
		}
		if err := as.Delete(ctx, boundaryID); err != nil {
			t.Fatal(err)
		}
		if deleted := request("GET", historyURL, nil); deleted.Code != http.StatusOK ||
			strings.Contains(deleted.Body.String(), ">history-fixture-02</a>") {
			t.Fatalf("deleted article appears in history: status %d", deleted.Code)
		}
	})
	t.Run("article sort order", func(t *testing.T) {
		for i := 1; i <= 21; i++ {
			title := fmt.Sprintf("sort-fixture-%02d", i)
			if err := as.Save(ctx, articles.Article{
				Title: title, URL: "https://example.org/" + title,
				PublishedAt: time.Date(2025, 1, i, 0, 0, 0, 0, time.UTC),
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, tc := range []struct {
			query         string
			first, second string
		}{
			{"", "01", "02"},
			{"&sort=newest", "21", "20"},
		} {
			w := request("GET", "/?q=sort-fixture"+tc.query+"&view=all", nil)
			first := strings.Index(w.Body.String(), ">sort-fixture-"+tc.first+"</a>")
			second := strings.Index(w.Body.String(), ">sort-fixture-"+tc.second+"</a>")
			if w.Code != http.StatusOK || first < 0 || second <= first ||
				!strings.Contains(w.Body.String(), "page=2&amp;q=sort-fixture"+strings.ReplaceAll(tc.query, "&", "&amp;")) {
				t.Fatalf("unexpected sort order or pagination for %q (status %d)", tc.query, w.Code)
			}
		}

		for _, q := range []articles.Query{
			{Text: "sort-fixture", ForwardOnly: true},
			{Text: "sort-fixture", View: "unread", ForwardOnly: true},
		} {
			first, err := as.Search(ctx, q)
			if err != nil || first.Total != 0 || len(first.Items) != 20 || !first.HasNext {
				t.Fatalf("unexpected forward page: %+v, %v", first, err)
			}
			if q.View == "unread" {
				q.AnchorID = first.Items[19].ID
			} else {
				q.Page = 2
			}
			last, err := as.Search(ctx, q)
			if err != nil || last.Total != 0 || len(last.Items) != 1 || last.HasNext ||
				last.Items[0].Title != "sort-fixture-21" {
				t.Fatalf("unexpected last forward page: %+v, %v", last, err)
			}
		}

		const unread = "/?q=sort-fixture&view=unread"
		fixture, err := as.Search(ctx, articles.Query{Text: "sort-fixture"})
		if err != nil || len(fixture.Items) != 20 {
			t.Fatalf("load unread page fixture: %v", err)
		}
		boundary := fixture.Items[19].ID
		firstPage := request("GET", unread, nil)
		nextLink := regexp.MustCompile(`data-next-articles href="([^"]+)"`).FindStringSubmatch(firstPage.Body.String())
		if firstPage.Code != http.StatusOK || len(nextLink) != 2 {
			t.Fatalf("unread next link missing: %d", firstPage.Code)
		}
		nextURL, err := url.Parse(strings.ReplaceAll(nextLink[1], "&amp;", "&"))
		if err != nil {
			t.Fatal(err)
		}
		if q := nextURL.Query(); q.Get("after") != fmt.Sprint(boundary) ||
			q.Get("published_at") != fixture.Items[19].PublishedAt.UTC().Format(time.RFC3339Nano) {
			t.Fatalf("unexpected next page boundary: %s", nextURL)
		}
		// Reading every visible row must not skip the remaining unread row on the next page.
		form := url.Values{"return_to": {unread}}
		for _, item := range fixture.Items {
			form.Add("id", fmt.Sprint(item.ID))
		}
		if w := request("POST", "/articles/mark-read", nil); w.Code != http.StatusBadRequest {
			t.Fatalf("empty bulk read returned %d", w.Code)
		}
		if w := request("POST", "/articles/mark-read", form); w.Code != http.StatusSeeOther {
			t.Fatalf("bulk read returned %d: %s", w.Code, w.Body.String())
		}
		if w := request("GET", unread, nil); w.Code != http.StatusOK ||
			strings.Count(w.Body.String(), "data-article-id=") != 1 ||
			!strings.Contains(w.Body.String(), "data-mark-loaded-read") {
			t.Fatalf("unread view should contain one remaining article and the bulk read button: %d", w.Code)
		}
		nextUnread := request("GET", fmt.Sprintf("%s&after=%d", unread, boundary), nil)
		if nextUnread.Code != http.StatusOK || !strings.Contains(nextUnread.Body.String(), ">sort-fixture-21</a>") {
			t.Fatalf("next page lost the remaining unread article: %d", nextUnread.Code)
		}
		if _, err := owner.ExecContext(ctx, `UPDATE articles.entries SET published_at=published_at+interval '2 days' WHERE id=$1`, boundary); err != nil {
			t.Fatal(err)
		}
		nextUnread = request("GET", nextURL.String(), nil)
		if nextUnread.Code != http.StatusOK || !strings.Contains(nextUnread.Body.String(), ">sort-fixture-21</a>") {
			t.Fatalf("corrected publication time moved the boundary: %d", nextUnread.Code)
		}
		req := httptest.NewRequest(http.MethodGet, nextURL.String(), nil)
		req.Header.Set("Accept", "text/vnd.rss-reader.article-page+html")
		fragment := httptest.NewRecorder()
		app.routes().ServeHTTP(fragment, req)
		if fragment.Code != http.StatusOK ||
			!strings.Contains(fragment.Header().Get("Content-Type"), "text/vnd.rss-reader.article-page+html") ||
			strings.Count(fragment.Body.String(), "data-article-id=") != 1 ||
			!strings.Contains(fragment.Body.String(), ">sort-fixture-21</a>") ||
			strings.Contains(fragment.Body.String(), "id=\"reader-pane\"") ||
			strings.Contains(fragment.Body.String(), "subscription-manager") {
			t.Fatalf("unexpected article page fragment: %d %s", fragment.Code, fragment.Body.String())
		}

		reversedURL := *nextURL
		reversedQuery := reversedURL.Query()
		reversedQuery.Del("after")
		reversedQuery.Set("before", fmt.Sprint(boundary))
		reversedQuery.Set("sort", "newest")
		reversedURL.RawQuery = reversedQuery.Encode()
		reversed := request("GET", reversedURL.String(), nil)
		if reversed.Code != http.StatusOK || !strings.Contains(reversed.Body.String(), ">sort-fixture-21</a>") {
			t.Fatalf("previous page lost the remaining article in newest-first order: %d", reversed.Code)
		}
	})

	t.Run("bulk read leaves articles added after rendering unread", func(t *testing.T) {
		const path = "/?q=bulk-read-snapshot&view=unread"
		saveArticle := func(label string) {
			t.Helper()
			if err := as.Save(ctx, articles.Article{
				Title: "bulk-read-snapshot " + label,
				URL:   "https://example.org/bulk-read-snapshot/" + label,
			}); err != nil {
				t.Fatal(err)
			}
		}
		saveArticle("first")
		saveArticle("second")
		before := request("GET", path, nil)
		if before.Code != http.StatusOK {
			t.Fatalf("load initial view: %d", before.Code)
		}
		matches := regexp.MustCompile(`data-article-id="([0-9]+)"`).FindAllStringSubmatch(before.Body.String(), -1)
		if len(matches) != 2 || strings.Contains(before.Body.String(), "data-next-articles") {
			t.Fatal("expected exactly two articles and no next page in the initial view")
		}

		saveArticle("new-arrival")
		form := url.Values{"id": {matches[0][1], matches[1][1], matches[1][1]}}
		for _, want := range []int64{2, 0} {
			req := httptest.NewRequest("POST", "/articles/mark-read", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Accept", "application/json")
			w := httptest.NewRecorder()
			app.routes().ServeHTTP(w, req)
			var result struct {
				Count int64 `json:"count"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); w.Code != http.StatusOK ||
				err != nil ||
				result.Count != want {
				t.Fatalf("bulk read count: status=%d, body=%s, want=%d", w.Code, w.Body.String(), want)
			}
		}
		after := request("GET", path, nil)
		if after.Code != http.StatusOK ||
			strings.Count(after.Body.String(), "data-article-id=") != 1 ||
			!strings.Contains(after.Body.String(), ">bulk-read-snapshot new-arrival</a>") {
			t.Fatalf("expected only the newly added article to remain unread: %d", after.Code)
		}
	})
}
