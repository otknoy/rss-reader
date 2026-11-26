package articles

// FullContent is a readable version of the original page, kept separately from the feed.
type FullContent struct{ Title, HTML, Text, URL string }

// ContentStatus is a lightweight view used while an open article awaits automatic extraction.
type ContentStatus struct {
	Fetched bool   `json:"fetched"`
	Pending bool   `json:"pending"`
	Error   string `json:"error"`
}
