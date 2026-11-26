package main

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"rss-reader/internal/feeds"
	"strings"
	"testing"
)

func opmlUpload(t *testing.T, content []byte, returnTo string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("opml", "subscriptions.opml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("return_to", returnTo); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/feeds/import", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func TestOPMLUploadValidation(t *testing.T) {
	app := &application{}
	for _, tc := range []struct {
		name, content string
		status        int
	}{
		{"invalid XML", "<opml><body>", http.StatusBadRequest},
		{"wrong file type", "<html><body/></html>", http.StatusBadRequest},
		{"oversized file", strings.Repeat("x", feeds.MaxOPMLBytes+1), http.StatusRequestEntityTooLarge},

		{
			"oversized request",
			strings.Repeat("x", feeds.MaxOPMLBytes+(128<<10)),
			http.StatusRequestEntityTooLarge,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			app.routes().ServeHTTP(w, opmlUpload(t, []byte(tc.content), "/"))
			if w.Code != tc.status {
				t.Fatalf("status=%d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/feeds/import", strings.NewReader(""))
	app.routes().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing upload: %d", w.Code)
	}
	req = opmlUpload(t, []byte(`<opml><body/></opml>`), "/")
	req.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	app.routes().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin import allowed: %d", w.Code)
	}
}
