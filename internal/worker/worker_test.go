package worker

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivanlee1999/scribe/internal/couch"
	"github.com/ivanlee1999/scribe/internal/recognize"
)

// One real SB payload — two points, no optional channels — taken from the golden vectors the
// sb package checks against the iPad's encoder. Using a real one rather than a fabricated one
// means this exercises the actual decode path.
const twoPointStroke = "534202000200000000060000006f7d405f7c42060000005f7c425f7c42"

// stubModel stands in for the recognizer. Vision models legitimately give different answers on
// different days, and a test that asserted on their output would be asserting on the weather.
type stubModel struct {
	mu     sync.Mutex
	calls  int
	images [][]byte
	text   string
	err    error
}

func (s *stubModel) Engine() string { return "stub" }

func (s *stubModel) Recognize(ctx context.Context, png []byte) (recognize.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.images = append(s.images, png)
	if s.err != nil {
		return recognize.Result{}, s.err
	}
	return recognize.Result{Text: s.text, Engine: "stub", Language: "en"}, nil
}

func (s *stubModel) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// live is a CouchDB to run against, or a skip. The fakes cannot prove CouchDB agrees with them —
// the revision rules especially — so this suite talks to a real one when there is one.
type live struct {
	client *couch.Client
	notes  string
	text   string
}

func requireCouch(t *testing.T) live {
	t.Helper()
	url := os.Getenv("SCRIBE_TEST_COUCH_URL")
	if url == "" {
		t.Skip("SCRIBE_TEST_COUCH_URL is not set")
	}

	client := couch.New(url, os.Getenv("SCRIBE_TEST_COUCH_USER"), os.Getenv("SCRIBE_TEST_COUCH_PASSWORD"))
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	l := live{client: client, notes: "scribe_notes_" + suffix, text: "scribe_text_" + suffix}

	// The client has no "create database" call, because the service never creates one — it is a
	// reader of the library and a writer of documents, not an administrator. The fixture needs
	// one, so it goes over raw HTTP.
	for _, database := range []string{l.notes, l.text} {
		if err := createDatabase(url, database); err != nil {
			t.Fatalf("creating %s: %v", database, err)
		}
	}
	t.Cleanup(func() {
		for _, database := range []string{l.notes, l.text} {
			_ = deleteDatabase(url, database)
		}
	})
	return l
}

func createDatabase(baseURL, name string) error { return databaseRequest("PUT", baseURL, name) }
func deleteDatabase(baseURL, name string) error { return databaseRequest("DELETE", baseURL, name) }

func databaseRequest(method, baseURL, name string) error {
	req, err := newRequest(method, strings.TrimRight(baseURL, "/")+"/"+name)
	if err != nil {
		return err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 && resp.StatusCode != 412 && resp.StatusCode != 404 {
		return fmt.Errorf("%s %s: %d", method, name, resp.StatusCode)
	}
	return nil
}

// putPage stores a page document with one real stroke on it.
func (l live) putPage(t *testing.T, pageID, updatedAt string, strokes int) {
	t.Helper()
	raw, err := hex.DecodeString(twoPointStroke)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.StdEncoding.EncodeToString(raw)

	page := map[string]any{
		"type":       "page",
		"notebookId": "book-1",
		"title":      "A page",
		"updatedAt":  updatedAt,
		"pageWidth":  1400,
		"pageHeight": 1980,
		"strokes":    []map[string]any{},
	}
	list := make([]map[string]any, 0, strokes)
	for i := 0; i < strokes; i++ {
		list = append(list, map[string]any{
			"pen": "BALLPEN", "size": 4, "pointsData": payload,
		})
	}
	page["strokes"] = list

	l.upsert(t, l.notes, "page:"+pageID, page)
}

// upsert writes a document, carrying over the current revision when there is one.
func (l live) upsert(t *testing.T, database, id string, doc map[string]any) {
	t.Helper()
	ctx := context.Background()
	var current map[string]any
	if err := l.client.Get(ctx, database, id, &current); err == nil {
		if rev, ok := current["_rev"].(string); ok {
			doc["_rev"] = rev
		}
	}
	status, err := l.client.Put(ctx, database, id, doc)
	if err != nil {
		t.Fatalf("put %s: %v", id, err)
	}
	if status >= 400 {
		t.Fatalf("put %s: status %d", id, status)
	}
}

func (l live) readText(t *testing.T, pageID string) (PageText, bool) {
	t.Helper()
	var text PageText
	err := l.client.Get(context.Background(), l.text, "pagetext:"+pageID, &text)
	if err != nil {
		return PageText{}, false
	}
	return text, true
}

func newWorker(t *testing.T, l live, model recognize.Recognizer) *Worker {
	t.Helper()
	state := NewFileStore(filepath.Join(t.TempDir(), "state.json"))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(l.client, model, state, Config{
		NotesDatabase: l.notes,
		TextDatabase:  l.text,
		DeviceID:      "server",
		Batch:         100,
	}, log)
}

func TestAPageIsReadAndPublished(t *testing.T) {
	l := requireCouch(t)
	l.putPage(t, "page-1", "2026-08-18T04:00:00.000Z", 1)
	model := &stubModel{text: "milk, eggs"}
	w := newWorker(t, l, model)

	if _, err := w.Once(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}

	text, ok := l.readText(t, "page-1")
	if !ok {
		t.Fatal("nothing was published")
	}
	if text.Text != "milk, eggs" {
		t.Errorf("text = %q", text.Text)
	}
	// The clock has to be the page's own, copied verbatim — that is what every other reader
	// compares against to decide whether this text still describes the ink.
	if text.RecognizedClock != "2026-08-18T04:00:00.000Z" {
		t.Errorf("recognizedClock = %q, want the page's updatedAt", text.RecognizedClock)
	}
	if text.NotebookID != "book-1" || text.PageTitle != "A page" {
		t.Errorf("the plugin's filing fields are missing: %+v", text)
	}
	if text.UpdatedBy != "server" {
		t.Errorf("updatedBy = %q", text.UpdatedBy)
	}
}

func TestAPageIsNotReadTwiceForNothing(t *testing.T) {
	// The expensive mistake: re-reading unchanged pages on every pass. On this hardware that is
	// the difference between an idle service and one pinned at 100% forever.
	l := requireCouch(t)
	l.putPage(t, "page-1", "2026-08-18T04:00:00.000Z", 1)
	model := &stubModel{text: "milk"}
	w := newWorker(t, l, model)

	for i := 0; i < 3; i++ {
		if _, err := w.Once(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	if model.count() != 1 {
		t.Errorf("the model ran %d times for one unchanged page", model.count())
	}
}

func TestEditingAPageMakesItReadableAgain(t *testing.T) {
	l := requireCouch(t)
	l.putPage(t, "page-1", "2026-08-18T04:00:00.000Z", 1)
	model := &stubModel{text: "milk"}
	w := newWorker(t, l, model)
	if _, err := w.Once(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// More ink, and a later clock.
	l.putPage(t, "page-1", "2026-08-18T06:00:00.000Z", 2)
	model.text = "milk, eggs"
	if _, err := w.Once(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	if model.count() != 2 {
		t.Errorf("the model ran %d times across an edit", model.count())
	}
	text, _ := l.readText(t, "page-1")
	if text.Text != "milk, eggs" {
		t.Errorf("text = %q, want the newer reading", text.Text)
	}
	if text.RecognizedClock != "2026-08-18T06:00:00.000Z" {
		t.Errorf("recognizedClock = %q, want the new page clock", text.RecognizedClock)
	}
}

func TestATabletsReadingOfNewerInkIsLeftAlone(t *testing.T) {
	// The rule that keeps three writers from fighting. The tablet has read ink this server has
	// not seen; publishing over it would lose the better answer and invite a rewrite back.
	l := requireCouch(t)
	l.putPage(t, "page-1", "2026-08-18T04:00:00.000Z", 1)
	l.upsert(t, l.text, "pagetext:page-1", map[string]any{
		"pageId":          "page-1",
		"text":            "from the iPad",
		"engine":          "vision",
		"recognizedClock": "2026-08-18T09:00:00.000Z",
		"updatedAt":       "2026-08-18T09:00:01.000Z",
		"updatedBy":       "ipad",
	})

	w := newWorker(t, l, &stubModel{text: "from the server"})
	if _, err := w.Once(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}

	text, _ := l.readText(t, "page-1")
	if text.Text != "from the iPad" {
		t.Errorf("text = %q — the server overwrote a reading of newer ink", text.Text)
	}
}

func TestAPageWithNoInkIsRecordedAsEmptyRatherThanSkipped(t *testing.T) {
	// Otherwise it is revisited on every pass forever, and no reader can tell "blank page" from
	// "nobody has looked at this yet".
	l := requireCouch(t)
	l.putPage(t, "page-1", "2026-08-18T04:00:00.000Z", 0)
	model := &stubModel{text: "should not be called"}
	w := newWorker(t, l, model)

	if _, err := w.Once(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}

	text, ok := l.readText(t, "page-1")
	if !ok {
		t.Fatal("an empty page published nothing")
	}
	if text.Text != "" {
		t.Errorf("text = %q, want empty", text.Text)
	}
	if model.count() != 0 {
		t.Error("the model was asked to read a blank page")
	}
}

func TestOnlyPagesAreRead(t *testing.T) {
	// Notebooks and folders travel on the same feed and have no ink on them.
	l := requireCouch(t)
	l.upsert(t, l.notes, "notebook:book-1", map[string]any{
		"type": "notebook", "title": "Notes", "pageIds": []string{"page-1"},
	})
	l.upsert(t, l.notes, "folder:f1", map[string]any{"type": "folder", "title": "Work"})
	model := &stubModel{text: "milk"}
	w := newWorker(t, l, model)

	if _, err := w.Once(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}

	if model.count() != 0 {
		t.Errorf("the model was handed %d non-page documents", model.count())
	}
}

func TestAFailingModelLeavesThePageForNextTime(t *testing.T) {
	// A model that is restarting must not cost the page silently — nothing may be published,
	// and the next edit has to bring it back.
	l := requireCouch(t)
	l.putPage(t, "page-1", "2026-08-18T04:00:00.000Z", 1)
	model := &stubModel{err: fmt.Errorf("model is down")}
	w := newWorker(t, l, model)

	if _, err := w.Once(context.Background()); err != nil {
		t.Fatalf("a failing page took down the whole pass: %v", err)
	}
	if _, ok := l.readText(t, "page-1"); ok {
		t.Error("a failed reading was published anyway")
	}

	model.err = nil
	model.text = "milk"
	l.putPage(t, "page-1", "2026-08-18T07:00:00.000Z", 1)
	if _, err := w.Once(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if text, ok := l.readText(t, "page-1"); !ok || text.Text != "milk" {
		t.Error("the page never recovered")
	}
}

func TestTheModelIsHandedARenderedPage(t *testing.T) {
	l := requireCouch(t)
	l.putPage(t, "page-1", "2026-08-18T04:00:00.000Z", 1)
	model := &stubModel{text: "milk"}
	w := newWorker(t, l, model)

	if _, err := w.Once(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}

	if len(model.images) != 1 {
		t.Fatalf("the model got %d images", len(model.images))
	}
	// A PNG, not the raw stroke data — the whole decode-and-render path ran.
	if !strings.HasPrefix(string(model.images[0][1:4]), "PNG") {
		t.Errorf("the model was handed something that is not a PNG: %x", model.images[0][:8])
	}
}

func TestTheCheckpointSurvivesARestart(t *testing.T) {
	l := requireCouch(t)
	l.putPage(t, "page-1", "2026-08-18T04:00:00.000Z", 1)

	statePath := filepath.Join(t.TempDir(), "state.json")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := Config{NotesDatabase: l.notes, TextDatabase: l.text, DeviceID: "server", Batch: 100}
	model := &stubModel{text: "milk"}

	first := New(l.client, model, NewFileStore(statePath), cfg, log)
	if _, err := first.Once(context.Background()); err != nil {
		t.Fatalf("first: %v", err)
	}

	// A fresh worker on the same state file must pick up where the last one stopped, rather
	// than replaying the library and re-reading every page.
	second := New(l.client, model, NewFileStore(statePath), cfg, log)
	if _, err := second.Once(context.Background()); err != nil {
		t.Fatalf("second: %v", err)
	}

	if model.count() != 1 {
		t.Errorf("the model ran %d times across a restart with no new edits", model.count())
	}
}

// The two HTTP helpers the fixture needs, kept apart from the tests themselves.

func httpClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

func newRequest(method, url string) (*http.Request, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	if user := os.Getenv("SCRIBE_TEST_COUCH_USER"); user != "" {
		req.SetBasicAuth(user, os.Getenv("SCRIBE_TEST_COUCH_PASSWORD"))
	}
	return req, nil
}
