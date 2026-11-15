package worker

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivanlee1999/scribe/internal/couch"
	"github.com/ivanlee1999/scribe/internal/recognize"
)

// Only protocol plumbing is faked; the source decode/render and durable state are real.
type queueFixture struct {
	mu      sync.Mutex
	page    Page
	text    *PageText
	changes []string
	writes  int
	trashed bool
	image   *PageImage
}

func (f *queueFixture) edit(fn func(*Page)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.page)
	f.changes = append(f.changes, f.page.ID)
	f.page.Rev = fmt.Sprintf("%d-ink", len(f.changes))
}
func (f *queueFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/notes/_changes":
		since, _ := strconv.Atoi(r.URL.Query().Get("since"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		end := min(len(f.changes), since+limit)
		rows := []map[string]any{}
		for i := since; i < end; i++ {
			rows = append(rows, map[string]any{"id": f.changes[i], "seq": i + 1})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"last_seq": end, "results": rows})
	case r.URL.Path == "/notes/page:p":
		_ = json.NewEncoder(w).Encode(f.page)
	case r.URL.Path == "/notes/notebook:b":
		notebook := map[string]any{"pageIds": []string{"p"}}
		if f.trashed {
			notebook["deletedAt"] = "2026-11-14T00:00:00Z"
		}
		_ = json.NewEncoder(w).Encode(notebook)
	case r.URL.Path == "/images/pageimage:p" && r.Method == "GET":
		if f.image == nil {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(f.image)
	case r.URL.Path == "/images/pageimage:p" && r.Method == "PUT":
		var image PageImage
		_ = json.NewDecoder(r.Body).Decode(&image)
		image.Rev = "1-image"
		f.image = &image
		w.WriteHeader(201)
	case r.URL.Path == "/text/pagetext:p" && r.Method == "GET":
		if f.text == nil {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(f.text)
	case r.URL.Path == "/text/pagetext:p" && r.Method == "PUT":
		var text PageText
		_ = json.NewDecoder(r.Body).Decode(&text)
		if f.text != nil && text.Rev != f.text.Rev {
			w.WriteHeader(409)
			return
		}
		f.writes++
		text.Rev = fmt.Sprintf("%d-text", f.writes)
		f.text = &text
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{ "ok": true }`)
	default:
		w.WriteHeader(404)
	}
}
func setupQueue(t *testing.T, model recognize.Recognizer, quiet time.Duration) (*queueFixture, *Worker) {
	t.Helper()
	raw, _ := hex.DecodeString(twoPointStroke)
	f := &queueFixture{page: Page{ID: "page:p", NotebookID: "b", UpdatedAt: "2026-11-14T00:00:00Z", PageWidth: 1400, PageHeight: 1980,
		Strokes: []Stroke{{Pen: "BALLPEN", Size: 4, PointsData: base64.StdEncoding.EncodeToString(raw)}}}}
	f.edit(func(*Page) {})
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	w := New(couch.New(server.URL, "", ""), model, NewFileStore(filepath.Join(t.TempDir(), "state.json")), Config{
		NotesDatabase: "notes", TextDatabase: "text", DeviceID: "server", Quiet: quiet,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }
	return f, w
}
func pass(t *testing.T, w *Worker) {
	t.Helper()
	if _, err := w.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func advance(w *Worker, by time.Duration) {
	now := w.now().Add(by)
	w.now = func() time.Time { return now }
}

func TestQueueCoalescesAndWaitsForQuiet(t *testing.T) {
	model := &stubModel{text: "hello"}
	f, w := setupQueue(t, model, 5*time.Second)
	pass(t, w)
	advance(w, 4*time.Second)
	f.edit(func(p *Page) { p.Strokes = append(p.Strokes, p.Strokes[0]) })
	f.edit(func(p *Page) { p.Strokes = append(p.Strokes, p.Strokes[0]) })
	pass(t, w)
	advance(w, 4*time.Second)
	pass(t, w)
	if model.count() != 0 {
		t.Fatal("recognized before edits settled")
	}
	advance(w, time.Second)
	pass(t, w)
	if model.count() != 1 || f.text == nil || f.text.SourceRev != "3-ink" || f.text.PipelineVersion == "" {
		t.Fatalf("not latest settled source: %+v", f.text)
	}
	state, _ := w.store.Load()
	if len(state.Pending) != 0 || state.Seq != "3" {
		t.Fatalf("bad checkpoint/queue: %+v", state)
	}
}
func TestMetadataRefreshDoesNotCallModelAndSameClockInkDoes(t *testing.T) {
	model := &stubModel{text: "hello"}
	f, w := setupQueue(t, model, -1)
	pass(t, w)
	originalHash := f.text.SourceFingerprint
	f.edit(func(p *Page) { p.Title = "Renamed"; p.UpdatedAt = "2026-11-14T01:00:00Z" })
	pass(t, w)
	if model.count() != 1 || f.text.PageTitle != "Renamed" || f.text.SourceRev != "2-ink" || f.text.SourceFingerprint != originalHash {
		t.Fatal("metadata edit caused inference or stale projection")
	}
	f.edit(func(p *Page) { p.Strokes = append(p.Strokes, p.Strokes[0]); p.UpdatedAt = "2020-01-01T00:00:00Z" })
	pass(t, w)
	if model.count() != 2 || f.text.SourceFingerprint == originalHash {
		t.Fatal("ink change hidden by clock")
	}
}
func TestRetrySurvivesRestartWithoutAnotherEdit(t *testing.T) {
	model := &stubModel{err: fmt.Errorf("temporary outage")}
	f, w := setupQueue(t, model, -1)
	pass(t, w)
	state, _ := w.store.Load()
	if len(state.Pending) != 1 || state.Seq != "1" || state.Pending["page:p"].Attempts != 1 {
		t.Fatalf("failed work lost: %+v", state)
	}
	next := New(w.couch, model, w.store, w.cfg, w.log)
	next.now = w.now
	model.err = nil
	model.text = "recovered"
	pass(t, next)
	if model.count() != 1 {
		t.Fatal("ignored backoff")
	}
	advance(next, 10*time.Second)
	pass(t, next)
	if f.text == nil || f.text.Text != "recovered" || model.count() != 2 {
		t.Fatal("job did not recover after restart")
	}
}

type hookedModel struct {
	hook  func()
	calls int
}

func (m *hookedModel) Engine() string { return "hook" }
func (m *hookedModel) Recognize(context.Context, []byte) (recognize.Result, error) {
	m.calls++
	if m.hook != nil {
		m.hook()
		m.hook = nil
	}
	return recognize.Result{Text: "read", Engine: "hook"}, nil
}
func TestInkArrivingDuringInferenceNeverPublishesOldAnswer(t *testing.T) {
	model := &hookedModel{}
	f, w := setupQueue(t, model, -1)
	model.hook = func() { f.edit(func(p *Page) { p.Strokes = append(p.Strokes, p.Strokes[0]) }) }
	pass(t, w)
	if f.writes != 0 {
		t.Fatal("published obsolete ink")
	}
	pass(t, w)
	if f.text == nil || f.text.SourceRev != "2-ink" || model.calls != 2 {
		t.Fatal("latest ink not retried")
	}
}
func TestTrashDuringInferenceDoesNotPublish(t *testing.T) {
	model := &hookedModel{}
	f, w := setupQueue(t, model, -1)
	model.hook = func() { f.mu.Lock(); f.trashed = true; f.mu.Unlock() }
	pass(t, w)
	if f.writes != 0 {
		t.Fatal("published after trash")
	}
	pass(t, w)
	state, _ := w.store.Load()
	if len(state.Pending) != 0 {
		t.Fatal("trashed page kept retrying")
	}
}
func TestMalformedStrokeNeverReplacesPreviousText(t *testing.T) {
	model := &stubModel{text: "original"}
	f, w := setupQueue(t, model, -1)
	pass(t, w)
	f.edit(func(p *Page) { p.Strokes[0].PointsData = "not base64" })
	pass(t, w)
	if f.writes != 1 || f.text.Text != "original" || model.count() != 1 {
		t.Fatal("malformed source replaced good text")
	}
	state, _ := w.store.Load()
	if state.Pending["page:p"].Attempts != 1 {
		t.Fatal("malformed job not retained")
	}
}
func TestPipelineUpgradeReplaysCheckpointedPagesExactlyOnce(t *testing.T) {
	model := &stubModel{text: "upgraded"}
	f, w := setupQueue(t, model, -1)
	// Simulate a legacy caught-up deployment. No new page edit will arrive.
	if err := w.store.Save(State{Seq: "1"}); err != nil {
		t.Fatal(err)
	}
	pass(t, w)
	if model.count() != 1 || f.text == nil || !strings.HasPrefix(f.text.PipelineVersion, "scribe-v2:") {
		t.Fatal("upgrade skipped caught-up pages")
	}
	pass(t, w)
	if model.count() != 1 {
		t.Fatal("upgrade repeated")
	}
	// Changing the recognizer identity also initiates one safe replay.
	next := New(w.couch, &hookedModel{}, w.store, w.cfg, w.log)
	next.now = w.now
	next.cfg.Quiet = 0
	pass(t, next)
	if f.writes != 2 || f.text.Engine != "hook" {
		t.Fatal("model change did not replay")
	}
	pass(t, next)
	if f.writes != 2 {
		t.Fatal("model change repeated")
	}
}
func TestCorruptStateReplaysRatherThanKeepingPartialCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"seq":"999","pending":broken}`), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := NewFileStore(path).Load()
	if err != nil || state.Seq != "0" || len(state.Pending) != 0 {
		t.Fatalf("partial corrupt checkpoint survived: %+v %v", state, err)
	}
}

func TestRemoveWinsTombstonesPreventErasedInkRecognition(t *testing.T) {
	model := &stubModel{text: "original"}
	f, w := setupQueue(t, model, -1)
	f.edit(func(p *Page) { p.Strokes[0].ID = "erased" })
	pass(t, w)
	f.edit(func(p *Page) {
		p.DeletedStrokes = append(p.DeletedStrokes, struct {
			ID string `json:"id"`
		}{ID: "erased"})
		p.Strokes[0].PointsData = "malformed but deleted"
	})
	pass(t, w)
	if model.count() != 1 || f.text.Text != "" || f.text.SourceRev != "3-ink" {
		t.Fatalf("erased ink resurfaced: %+v", f.text)
	}
	// Tombstones for absent strokes do not alter the rendering input.
	hash := f.text.SourceFingerprint
	f.edit(func(p *Page) {
		p.DeletedStrokes = append(p.DeletedStrokes, struct {
			ID string `json:"id"`
		}{ID: "already absent"})
	})
	pass(t, w)
	if model.count() != 1 || f.text.SourceFingerprint != hash {
		t.Fatal("irrelevant tombstone caused inference")
	}
}

func TestErasingFinalStrokeReplacesRenderedImage(t *testing.T) {
	model := &stubModel{text: "original"}
	f, w := setupQueue(t, model, -1)
	w.cfg.ImagesDatabase = "images"
	f.edit(func(p *Page) { p.Strokes[0].ID = "erase-me" })
	pass(t, w)
	if f.image == nil || f.image.Width <= 1 || f.image.SourceRev != "2-ink" {
		t.Fatal("rendered ink storage was lost")
	}
	f.edit(func(p *Page) {
		p.DeletedStrokes = append(p.DeletedStrokes, struct {
			ID string `json:"id"`
		}{ID: "erase-me"})
	})
	pass(t, w)
	if f.image == nil || f.image.Width != 1 || f.image.Height != 1 || f.image.SourceRev != "3-ink" || f.image.Attachments[ImageAttachmentName].Data == "" {
		t.Fatal("erased ink image remained visible")
	}
	if model.count() != 1 {
		t.Fatal("paid model called for blank page")
	}
}
