package feeds

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"golang.org/x/net/html/charset"
)

// MaxOPMLBytes bounds uploaded subscription lists without requiring temporary files.
const MaxOPMLBytes = 2 << 20

// ErrInvalidOPML indicates an invalid or unsupported subscription document.
var ErrInvalidOPML = errors.New("OPMLファイルを読み込めませんでした")

// ErrOPMLTooLarge indicates the uploaded file exceeds the supported size.
var ErrOPMLTooLarge = errors.New("OPMLファイルは2MB以下にしてください")

// ImportResult summarizes merged subscriptions; reactivated subscriptions count as added.
type ImportResult struct{ Added, Existing, Skipped int }

type parsedOPML struct {
	feeds               []Feed
	duplicates, skipped int
}

func parseOPML(input io.Reader) (parsedOPML, error) {
	var result parsedOPML
	raw, err := io.ReadAll(io.LimitReader(input, MaxOPMLBytes+1))
	if err != nil {
		return result, err
	}
	if len(raw) > MaxOPMLBytes {
		return result, ErrOPMLTooLarge
	}
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	decoder.CharsetReader = charset.NewReaderLabel
	depth, bodyDepth, outlines := 0, 0, 0
	rootSeen, bodySeen := false, false
	seen := make(map[string]bool)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return parsedOPML{}, fmt.Errorf("%w: XMLの形式が不正です", ErrInvalidOPML)
		}
		switch token := token.(type) {
		case xml.StartElement:
			depth++
			if depth > 64 {
				return parsedOPML{}, fmt.Errorf("%w: 階層が深すぎます", ErrInvalidOPML)
			}
			if depth == 1 {
				if rootSeen || token.Name.Local != "opml" {
					return parsedOPML{}, ErrInvalidOPML
				}
				rootSeen = true
			}
			if depth == 2 && token.Name.Local == "body" {
				if bodySeen {
					return parsedOPML{}, ErrInvalidOPML
				}
				bodySeen = true
				bodyDepth = depth
			}
			if bodyDepth == 0 || token.Name.Local != "outline" {
				continue
			}
			outlines++
			if outlines > 10000 {
				return parsedOPML{}, fmt.Errorf("%w: outlineが多すぎます", ErrInvalidOPML)
			}
			var rawURL, title, text, kind string
			for _, attr := range token.Attr {
				switch strings.ToLower(attr.Name.Local) {
				case "xmlurl":
					rawURL = strings.TrimSpace(attr.Value)
				case "title":
					title = strings.TrimSpace(attr.Value)
				case "text":
					text = strings.TrimSpace(attr.Value)
				case "type":
					kind = strings.ToLower(attr.Value)
				}
			}
			if rawURL == "" {
				if kind == "rss" || kind == "atom" {
					result.skipped++
				}
				continue
			}
			if ValidateURL(rawURL) != nil {
				result.skipped++
				continue
			}
			if seen[rawURL] {
				result.duplicates++
				continue
			}
			seen[rawURL] = true
			if len(result.feeds) >= 1000 {
				return parsedOPML{}, fmt.Errorf("%w: フィードは1ファイル1000件までです", ErrInvalidOPML)
			}
			if title == "" {
				title = text
			}
			if title == "" {
				title = rawURL
			}
			result.feeds = append(result.feeds, Feed{URL: rawURL, Title: title})
		case xml.EndElement:
			if depth == bodyDepth {
				bodyDepth = 0
			}
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(token)) != "" {
				return parsedOPML{}, ErrInvalidOPML
			}
		}
	}
	if !rootSeen || !bodySeen || depth != 0 {
		return parsedOPML{}, ErrInvalidOPML
	}
	return result, nil
}

// ImportOPML validates the whole file before atomically merging subscriptions.
func (s *Store) ImportOPML(ctx context.Context, input io.Reader) (result ImportResult, err error) {
	parsed, err := parseOPML(input)
	if err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	result.Existing = parsed.duplicates
	result.Skipped = parsed.skipped
	for _, feed := range parsed.feeds {
		var id int64
		err := tx.QueryRowContext(ctx, `INSERT INTO feeds.subscriptions(url,title) VALUES($1,$2)
 ON CONFLICT(url) DO UPDATE SET active=true,next_fetch_at=NULL
 WHERE NOT feeds.subscriptions.active RETURNING id`, feed.URL, feed.Title).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			result.Existing++
			continue
		}
		if err != nil {
			return ImportResult{}, err
		}
		result.Added++
	}
	if err := tx.Commit(); err != nil {
		return ImportResult{}, err
	}
	return result, nil
}

type opmlOutline struct {
	Text  string `xml:"text,attr"`
	Title string `xml:"title,attr"`
	Type  string `xml:"type,attr"`
	URL   string `xml:"xmlUrl,attr"`
}
type opmlDocument struct {
	XMLName xml.Name `xml:"opml"`
	Version string   `xml:"version,attr"`
	Head    struct {
		Title string `xml:"title"`
	} `xml:"head"`
	Body struct {
		Outlines []opmlOutline `xml:"outline"`
	} `xml:"body"`
}

// ExportOPML writes active subscriptions in a stable, portable OPML 2.0 document.
func (s *Store) ExportOPML(ctx context.Context, output io.Writer) error {
	list, err := s.List(ctx)
	if err != nil {
		return err
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := strings.ToLower(list[i].Title), strings.ToLower(list[j].Title)
		if a == b {
			return list[i].URL < list[j].URL
		}
		return a < b
	})
	doc := opmlDocument{Version: "2.0"}
	doc.Head.Title = "RSS Reader subscriptions"
	for _, feed := range list {
		doc.Body.Outlines = append(doc.Body.Outlines, opmlOutline{Text: feed.Title, Title: feed.Title, Type: "rss", URL: feed.URL})
	}
	if _, err := io.WriteString(output, xml.Header); err != nil {
		return err
	}
	encoder := xml.NewEncoder(output)
	encoder.Indent("", "  ")
	return encoder.Encode(doc)
}
