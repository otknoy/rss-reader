package ingestion

import (
	"net/netip"
	"net/url"
	"rss-reader/internal/feeds"
	"testing"
)

func TestPublicIP(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1",
		"::1",
		"::ffff:127.0.0.1",
		"10.0.0.1",
		"172.16.0.1",
		"192.168.1.1",
		"169.254.169.254",
		"100.100.100.200",
		"0.0.0.0",
		"fc00::1",
		"fe80::1",
		"2001:db8::1",
	} {
		if publicIP(netip.MustParseAddr(raw)) {
			t.Errorf("accepted private/reserved IP %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicIP(netip.MustParseAddr(raw)) {
			t.Errorf("rejected public IP %s", raw)
		}
	}
}

func TestPlainText(t *testing.T) {
	raw := `<p>日本語 &amp; Go</p><script>alert(1)</script><style>body{}</style><p>全文検索</p>`
	if got := PlainText(raw); got != "日本語 & Go 全文検索" {
		t.Fatalf("got %q", got)
	}
}

func TestParseFeeds(t *testing.T) {
	base, _ := url.Parse("https://example.com/feed.xml")
	for _, raw := range []string{
		`<rss version="2.0"><channel><title>購読</title><link>https://example.com</link><description>test</description><item><guid>stable-id</guid><title>記事</title><link>/entry</link><description>&lt;p&gt;日本語検索&lt;/p&gt;</description></item></channel></rss>`,
		`<feed xmlns="http://www.w3.org/2005/Atom"><title>購読</title><id>feed-id</id><entry><id>stable-id</id><title>記事</title><link href="/entry"/><content type="html">&lt;p&gt;日本語検索&lt;/p&gt;</content></entry></feed>`,
	} {
		parsed, err := parseFeed([]byte(raw), base, feeds.Feed{ID: 7})
		if err != nil {
			t.Fatal(err)
		}
		title, items := parsed.Title, parsed.Items
		if title != "購読" || len(items) != 1 {
			t.Fatalf("title=%s items=%v", title, items)
		}
		a := items[0]
		if a.ExternalID != "stable-id" ||
			a.SourceID != 7 ||
			a.URL != "https://example.com/entry" ||
			a.BodyText != "日本語検索" {
			t.Fatalf("unexpected article: %+v", a)
		}
	}
	if _, err := parseFeed([]byte("not a feed"), base, feeds.Feed{}); err == nil {
		t.Fatal("invalid feed accepted")
	}
}
