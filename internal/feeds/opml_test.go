package feeds

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParseNestedOPML(t *testing.T) {
	input := `<?xml version="1.0" encoding="UTF-8"?><opml version="2.0"><head><title>購読一覧</title></head><body>
 <outline text="フォルダー"><outline title="日本語 &amp; Go" text="別名" type="rss" xmlUrl="https://example.com/feed?a=1&amp;b=2"/>
 <outline text="入れ子"><outline text="Atom" xmlUrl=" https://example.com/atom "/></outline></outline>
 <outline xmlUrl="https://example.com/atom"/><outline type="rss"/>
 <outline xmlUrl="javascript:alert(1)"/><outline xmlUrl="file:///etc/passwd"/><outline xmlUrl="https://user:pass@example.com/feed"/>
 <outline type="link" url="https://example.com"/></body></opml>`
	parsed, err := parseOPML(strings.NewReader("\ufeff" + input))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.feeds) != 2 || parsed.duplicates != 1 || parsed.skipped != 4 {
		t.Fatalf("unexpected result: %+v", parsed)
	}
	if parsed.feeds[0].Title != "日本語 & Go" ||
		parsed.feeds[0].URL != "https://example.com/feed?a=1&b=2" ||
		parsed.feeds[1].Title != "Atom" {
		t.Fatalf("incorrect attributes: %+v", parsed.feeds)
	}
}

func TestOPMLRejectsInvalidDocuments(t *testing.T) {
	for _, input := range []string{
		"",
		`<html><body/></html>`,
		`<opml/>`,
		`<opml><body><outline xmlUrl="https://example.com"/></body>`,
		`<opml><body/></opml><opml><body/></opml>`,
		`<opml><body/><body/></opml>`,
		`<!DOCTYPE opml [<!ENTITY xxe SYSTEM "file:///etc/passwd">]><opml><body><outline text="&xxe;"/></body></opml>`,
		"<opml><body>" + strings.Repeat("<outline>", 65) + strings.Repeat("</outline>", 65) + "</body></opml>",
	} {
		if _, err := parseOPML(strings.NewReader(input)); !errors.Is(err, ErrInvalidOPML) {
			t.Errorf("accepted invalid input %q: %v", input, err)
		}
	}
	if _, err := parseOPML(strings.NewReader(strings.Repeat("x", MaxOPMLBytes+1))); !errors.Is(err, ErrOPMLTooLarge) {
		t.Fatalf("oversized file: %v", err)
	}
	var many strings.Builder
	many.WriteString("<opml><body>")
	for i := range 1001 {
		if _, err := fmt.Fprintf(&many, `<outline xmlUrl="https://example.com/%d"/>`, i); err != nil {
			t.Fatal(err)
		}
	}
	many.WriteString("</body></opml>")
	if _, err := parseOPML(strings.NewReader(many.String())); !errors.Is(err, ErrInvalidOPML) {
		t.Fatalf("feed count limit ignored: %v", err)
	}
}

func TestOPMLLegacyEncodingAndEmptyExport(t *testing.T) {
	encoded := `<?xml version="1.0" encoding="ISO-8859-1"?><opml><body><outline text="Caf` + "\xe9" + `" xmlUrl="https://example.com/feed"/></body></opml>`
	result, err := parseOPML(strings.NewReader(encoded))
	if err != nil || len(result.feeds) != 1 || result.feeds[0].Title != "Café" {
		t.Fatalf("encoding: %+v %v", result, err)
	}
	result, err = parseOPML(strings.NewReader(`<opml version="2.0"><head/><body/></opml>`))
	if err != nil || len(result.feeds) != 0 {
		t.Fatalf("empty OPML: %+v %v", result, err)
	}
}
