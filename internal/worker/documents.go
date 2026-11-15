package worker

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Page is the slice of a `page:` document this service reads.
//
// Everything else in a page document — images, tombstones, backgrounds, geometry — belongs to
// the tablets' sync protocol and is none of this service's business. Decoding only what is
// needed also means a protocol addition cannot break the server by surprise.
type Page struct {
	ID         string   `json:"_id"`
	Rev        string   `json:"_rev"`
	NotebookID string   `json:"notebookId"`
	Title      string   `json:"title"`
	UpdatedAt  string   `json:"updatedAt"`
	PageWidth  float32  `json:"pageWidth"`
	PageHeight float32  `json:"pageHeight"`
	Strokes    []Stroke `json:"strokes"`
}

// Stroke is one pen movement as the wire carries it.
type Stroke struct {
	Pen  string  `json:"pen"`
	Size float32 `json:"size"`
	// PointsData is base64 of the SB binary; see the sb package.
	PointsData string `json:"pointsData"`
}

// PageText is a `pagetext:` document — the contract in bopa's docs/recognized-text.md, which the
// two tablet apps and the Obsidian plugin all implement against.
type PageText struct {
	ID                string `json:"_id"`
	Rev               string `json:"_rev,omitempty"`
	PageID            string `json:"pageId"`
	NotebookID        string `json:"notebookId,omitempty"`
	PageTitle         string `json:"pageTitle,omitempty"`
	Text              string `json:"text"`
	SourceRev         string `json:"sourceRev,omitempty"`
	SourceFingerprint string `json:"sourceFingerprint,omitempty"`
	PipelineVersion   string `json:"pipelineVersion,omitempty"`
	Engine            string `json:"engine"`
	Language          string `json:"language,omitempty"`
	// RecognizedClock is the page's updatedAt at the moment its ink was read, copied verbatim.
	// The page moving past it is what makes this text stale.
	RecognizedClock string `json:"recognizedClock"`
	UpdatedAt       string `json:"updatedAt"`
	UpdatedBy       string `json:"updatedBy"`
}

const (
	pagePrefix     = "page:"
	pageTextPrefix = "pagetext:"
)

func isPageID(id string) bool         { return strings.HasPrefix(id, pagePrefix) }
func pageTextID(pageID string) string { return pageTextPrefix + pageID }

// bareID strips the type prefix from a document id.
func bareID(id string) string {
	if i := strings.IndexByte(id, ':'); i >= 0 {
		return id[i+1:]
	}
	return id
}

// millis parses one of the ISO stamps these documents carry.
//
// Both spellings are accepted because both are written: Notable prints fractional seconds only
// when they are non-zero, matching java.time.Instant. Returning ok=false rather than zero
// matters — a stamp nobody can parse must not become "the beginning of time", which would mark
// its page stale forever and recognize it on a loop.
func millis(stamp string) (int64, bool) {
	if stamp == "" {
		return 0, false
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05Z07:00",
	} {
		if t, err := time.Parse(layout, stamp); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

// nowStamp is the format the tablets write, so every clock in the text database reads the same.
func nowStamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// stale reports whether text needs re-reading because its page has moved on.
//
// Unknown clocks resolve toward doing the work: text with no readable clock is treated as stale,
// because the cost of recognizing a page twice is a model run, and the cost of wrongly deciding
// it is current is text that never updates again.
func stale(pageUpdatedAt, recognizedClock string) bool {
	page, pageOK := millis(pageUpdatedAt)
	recognized, recognizedOK := millis(recognizedClock)
	if !pageOK || !recognizedOK {
		return true
	}
	return page > recognized
}

// supersedes reports whether local should replace remote.
//
// This is the guard from docs/recognized-text.md, and it is what keeps several writers — two
// tablets and now this server — from overwriting each other forever. Text describing newer ink
// always wins. For the same ink it comes down to which reading is newer, and identical text is
// never rewritten at all, because a write that changes nothing still wakes every reader of the
// change feed.
func supersedes(local, remote PageText) bool {
	localInk, localOK := millis(local.RecognizedClock)
	remoteInk, remoteOK := millis(remote.RecognizedClock)
	if !localOK {
		localInk = -1 << 62
	}
	if !remoteOK {
		remoteInk = -1 << 62
	}
	if localInk != remoteInk {
		return localInk > remoteInk
	}

	if local.Text == remote.Text && local.Engine == remote.Engine && local.Language == remote.Language {
		return false
	}

	localRun, ok := millis(local.UpdatedAt)
	if !ok {
		localRun = -1 << 62
	}
	remoteRun, ok := millis(remote.UpdatedAt)
	if !ok {
		remoteRun = -1 << 62
	}
	return localRun > remoteRun
}

// PageImage is the rendered ink of one page, stored so that a reader can see the writing beside
// the text it was read as. The two are separate documents in separate databases on purpose: the
// tablets replicate the text and have the ink already, and none of them should be made to pull
// back a picture of what they drew.
//
// The PNG rides as an inline attachment, which is CouchDB's own mechanism for binary content and
// keeps an image inseparable from the metadata describing it.
type PageImage struct {
	ID         string `json:"_id"`
	Rev        string `json:"_rev,omitempty"`
	PageID     string `json:"pageId"`
	NotebookID string `json:"notebookId,omitempty"`
	// Width and Height are the rendered pixels, so a reader can size an embed without decoding.
	Width  int `json:"width"`
	Height int `json:"height"`
	// RenderedClock is the page's own updatedAt at the moment the ink was drawn, exactly as
	// RecognizedClock is for the text. It is what makes an image comparable against the page.
	RenderedClock string `json:"renderedClock"`
	UpdatedAt     string `json:"updatedAt"`
	UpdatedBy     string `json:"updatedBy"`

	Attachments map[string]Attachment `json:"_attachments,omitempty"`
}

// Attachment is one inline CouchDB attachment: base64 in, base64 out.
type Attachment struct {
	ContentType string `json:"content_type"`
	Data        string `json:"data"`
}

// ImageAttachmentName is the attachment every PageImage carries its PNG under. Fixed rather than
// derived, so a reader can build the URL without first fetching the document.
const ImageAttachmentName = "page.png"

func pageImageID(pageID string) string { return "pageimage:" + pageID }

// inkFingerprint excludes filing, title, clocks and typed blocks. Array order is rendering order.
func inkFingerprint(page Page) string {
	source := struct {
		Width, Height float32
		Strokes       []Stroke
	}{page.PageWidth, page.PageHeight, page.Strokes}
	if source.Strokes == nil {
		source.Strokes = []Stroke{}
	}
	data, _ := json.Marshal(source)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}
