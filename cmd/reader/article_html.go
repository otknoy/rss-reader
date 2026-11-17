package main

import (
	"html/template"
	"net/url"
	"regexp"
	"rss-reader/internal/articles"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
)

// Only article formatting is allowed; publisher CSS, scripts, forms and embeds are removed.
var articlePolicy = func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("p", "br", "hr", "h2", "h3", "h4", "h5", "h6", "strong", "b", "em", "i", "s", "del", "small", "sup", "sub", "ul", "ol", "li", "blockquote", "pre", "code", "figure", "figcaption", "img", "a", "table", "thead", "tbody", "tfoot", "tr", "th", "td", "div", "span")
	p.AllowAttrs("href", "title").OnElements("a")
	p.AllowAttrs("src", "alt", "title").OnElements("img")
	p.AllowURLSchemes("http", "https")
	p.RequireParseableURLs(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	p.AllowAttrs("loading").Matching(regexp.MustCompile("^lazy$")).OnElements("img")
	return p
}()

// richArticle is the only conversion from publisher HTML to trusted template HTML.
func richArticle(a articles.Article) template.HTML {
	raw, baseURL := a.BodyHTML, a.URL
	if a.FullBodyHTML != "" {
		raw, baseURL = a.FullBodyHTML, a.FullSourceURL
	}
	if raw == "" {
		return ""
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return ""
	}
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for i, attr := range n.Attr {
				if (n.Data == "a" && attr.Key == "href") || (n.Data == "img" && attr.Key == "src") {
					n.Attr[i].Val = resolveArticleURL(base, attr.Val)
				}
			}
			if n.Data == "img" {
				var src string
				for _, key := range []string{"data-src", "data-original", "src"} {
					for _, attr := range n.Attr {
						if attr.Key == key && src == "" {
							src = resolveArticleURL(base, attr.Val)
						}
					}
				}
				attrs := n.Attr[:0]
				for _, attr := range n.Attr {
					if attr.Key != "src" && attr.Key != "loading" {
						attrs = append(attrs, attr)
					}
				}
				if src != "" {
					attrs = append(attrs, html.Attribute{Key: "src", Val: src})
				}
				attrs = append(attrs, html.Attribute{Key: "loading", Val: "lazy"})
				n.Attr = attrs
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(doc)
	var normalized strings.Builder
	if err := html.Render(&normalized, doc); err != nil {
		return ""
	}
	safe := articlePolicy.Sanitize(normalized.String())
	// The allowlist sanitization above is the trust boundary.
	return template.HTML(safe) // #nosec G203 -- sanitized by articlePolicy.
}

func resolveArticleURL(base *url.URL, raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	relative, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	resolved := base.ResolveReference(relative)
	if (resolved.Scheme != "http" && resolved.Scheme != "https") || resolved.Hostname() == "" || resolved.User != nil {
		return ""
	}
	return resolved.String()
}
