// Package worker is the loop: find pages whose ink has outrun their text, read them, publish.
package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	imagepkg "image"
	"image/color"
	"image/png"
	"log/slog"
	"time"

	"github.com/ivanlee1999/scribe/internal/couch"
	"github.com/ivanlee1999/scribe/internal/recognize"
	"github.com/ivanlee1999/scribe/internal/render"
	"github.com/ivanlee1999/scribe/internal/sb"
)

// Config is everything the worker needs that is not a dependency.
type Config struct {
	NotesDatabase string
	TextDatabase  string
	// ImagesDatabase holds rendered ink; empty disables it.
	ImagesDatabase string
	// DeviceID identifies this server in the documents it writes. It must differ from the
	// tablets' "boox" and "ipad" so a reader can tell who wrote what.
	DeviceID string
	// Poll is how long to wait after an empty change feed before asking again.
	Poll time.Duration
	// Batch caps how many changes are taken at a time.
	Batch int
	// MaxPageBytes refuses pages larger than this rather than trying to hold them. A page of
	// dense ink runs to megabytes and this is a 6W machine.
	MaxPageBytes int
	// Quiet delays recognition after the last observed edit; negative disables it in batch tests.
	Quiet time.Duration
}

// Store persists the checkpoint and unfinished jobs together.
type Store interface {
	Load() (State, error)
	Save(State) error
}

type Worker struct {
	couch      *couch.Client
	recognizer recognize.Recognizer
	store      Store
	cfg        Config
	log        *slog.Logger
	now        func() time.Time
}

func New(client *couch.Client, recognizer recognize.Recognizer, store Store, cfg Config, log *slog.Logger) *Worker {
	if cfg.Batch <= 0 {
		cfg.Batch = 200
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 30 * time.Second
	}
	if cfg.MaxPageBytes <= 0 {
		cfg.MaxPageBytes = 32 << 20
	}
	if cfg.Quiet == 0 {
		cfg.Quiet = 5 * time.Second
	}
	if cfg.Quiet < 0 {
		cfg.Quiet = 0
	}
	return &Worker{couch: client, recognizer: recognizer, store: store, cfg: cfg, log: log, now: time.Now}
}

// Run follows the library until the context is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	for {
		worked, err := w.Once(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A server that is down or a model that is restarting are both ordinary. Logging
			// and waiting beats exiting: this runs unattended, and a crash loop that empties a
			// checkpoint is worse than a quiet retry.
			w.log.Error("pass failed", "error", err)
		}
		// Straight back round while there is a backlog; the poll interval is for a quiet feed.
		if worked && err == nil {
			continue
		}
		wait := w.cfg.Poll
		if err == nil {
			if state, loadErr := w.store.Load(); loadErr == nil {
				for _, job := range state.Pending {
					due := time.UnixMilli(job.DueAt).Sub(w.now())
					if due < 0 {
						due = 0
					}
					if due < wait {
						wait = due
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// Once first durably queues a feed batch, then processes at most one settled page.
// A full feed batch is drained before inference so a backlog coalesces to latest pages.
func (w *Worker) Once(ctx context.Context) (bool, error) {
	state, err := w.store.Load()
	if err != nil {
		return false, fmt.Errorf("reading state: %w", err)
	}
	if state.Pending == nil {
		state.Pending = map[string]Pending{}
	}
	if state.PipelineVersion != w.pipelineVersion() {
		state.Seq = "0" // One idempotent backfill when configuration changes.
		state.PipelineVersion = w.pipelineVersion()
	}
	last, changes, err := w.couch.Changes(ctx, w.cfg.NotesDatabase, state.Seq, w.cfg.Batch)
	if err != nil {
		return false, fmt.Errorf("reading changes: %w", err)
	}
	for _, change := range changes {
		if !isPageID(change.ID) {
			continue
		}
		if change.Deleted {
			delete(state.Pending, change.ID)
			continue
		}
		state.Pending[change.ID] = Pending{DueAt: w.now().Add(w.cfg.Quiet).UnixMilli()}
	}
	state.Seq = last
	if err := w.store.Save(state); err != nil {
		return false, fmt.Errorf("queuing changes: %w", err)
	}
	if len(changes) == w.cfg.Batch {
		return true, nil
	}
	var pageID string
	var job Pending
	for id, candidate := range state.Pending {
		if candidate.DueAt <= w.now().UnixMilli() && (pageID == "" || candidate.DueAt < job.DueAt || (candidate.DueAt == job.DueAt && id < pageID)) {
			pageID, job = id, candidate
		}
	}
	if pageID == "" {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := w.handlePage(ctx, pageID); err != nil {
		if errors.Is(err, errSourceChanged) {
			job = Pending{DueAt: w.now().Add(w.cfg.Quiet).UnixMilli()}
		} else {
			job.Attempts++
			backoff := 10 * time.Second
			for i := 1; i < job.Attempts && backoff < 5*time.Minute; i++ {
				backoff *= 2
			}
			if backoff > 5*time.Minute {
				backoff = 5 * time.Minute
			}
			job.DueAt = w.now().Add(backoff).UnixMilli()
			if errors.Is(err, errContextUnavailable) {
				w.log.Debug("page context unavailable", "page", pageID, "attempt", job.Attempts)
			} else {
				w.log.Error("page deferred", "page", pageID, "attempt", job.Attempts, "error", err)
			}
		}
		state.Pending[pageID] = job
	} else {
		delete(state.Pending, pageID)
	}
	if err := w.store.Save(state); err != nil {
		return false, fmt.Errorf("saving jobs: %w", err)
	}
	return len(state.Pending) > 0 && w.cfg.Quiet == 0 && job.Attempts == 0, nil
}

var errSourceChanged = errors.New("source changed during recognition")
var errContextUnavailable = errors.New("page context unavailable")

func (w *Worker) pipelineVersion() string {
	identity := w.recognizer.Engine()
	if versioned, ok := w.recognizer.(interface{ PipelineVersion() string }); ok {
		identity = versioned.PipelineVersion()
	}
	return fmt.Sprintf("scribe-v2:%x", sha256.Sum256([]byte("render-v1:"+identity)))
}

// handlePage reads one page if its text has fallen behind its ink.
func (w *Worker) handlePage(ctx context.Context, documentID string) error {
	var page Page
	if err := w.couch.Get(ctx, w.cfg.NotesDatabase, documentID, &page); err != nil {
		if errors.Is(err, couch.ErrNotFound) {
			return nil
		}
		return err
	}

	pageID := bareID(documentID)
	textID := pageTextID(pageID)

	var existing PageText
	haveExisting := true
	if err := w.couch.Get(ctx, w.cfg.TextDatabase, textID, &existing); err != nil {
		if !errors.Is(err, couch.ErrNotFound) {
			return err
		}
		haveExisting = false
	}

	live, err := w.pageIsLive(ctx, page)
	if err != nil {
		return err
	}
	if !live {
		return errContextUnavailable
	}
	fingerprint := inkFingerprint(page)
	pipeline := w.pipelineVersion()
	var result recognize.Result
	var image render.Result
	if haveExisting && existing.SourceFingerprint == fingerprint && existing.PipelineVersion == pipeline {
		if existing.SourceRev == page.Rev && existing.PageTitle == page.Title && existing.NotebookID == page.NotebookID {
			return w.backfillImage(ctx, page, pageID)
		}
		result = recognize.Result{Text: existing.Text, Engine: existing.Engine, Language: existing.Language, Format: existing.Format}
	} else {
		strokes, err := w.decode(page)
		if err != nil {
			return err
		}
		image, err = render.Render(strokes, render.DefaultOptions(page.PageWidth, page.PageHeight))
		if err != nil {
			return fmt.Errorf("rendering: %w", err)
		}
		result, err = w.read(ctx, image, page)
		if err != nil {
			return err
		}
	}
	var latest Page
	if err := w.couch.Get(ctx, w.cfg.NotesDatabase, documentID, &latest); err != nil {
		if errors.Is(err, couch.ErrNotFound) {
			return nil
		}
		return err
	}
	if inkFingerprint(latest) != fingerprint || latest.NotebookID != page.NotebookID {
		return errSourceChanged
	}
	text := PageText{
		ID: textID, Rev: existing.Rev, PageID: pageID, NotebookID: latest.NotebookID,
		PageTitle: latest.Title, Text: result.Text, Format: result.Format, Engine: result.Engine, Language: result.Language,
		RecognizedClock: latest.UpdatedAt, UpdatedAt: nowStamp(), UpdatedBy: w.cfg.DeviceID,
		SourceRev: latest.Rev, SourceFingerprint: fingerprint, PipelineVersion: pipeline,
	}

	if err := w.validateSource(ctx, text); err != nil {
		return err
	}
	// Save successful recognition before trying its image. A retry can then backfill the
	// image from the same source without paying for recognition again.
	if err := w.publish(ctx, text); err != nil {
		return err
	}
	if image.Empty || len(image.PNG) > 0 {
		return w.publishImage(ctx, image, latest, pageID, latest.UpdatedAt)
	}
	return w.backfillImage(ctx, latest, pageID)
}

// decode turns a page's stroke payloads into geometry.
func (w *Worker) decode(page Page) ([]render.Stroke, error) {
	strokes := make([]render.Stroke, 0, len(page.Strokes))
	total := 0
	for i, stroke := range activeStrokes(page) {
		total += len(stroke.PointsData)
		if total > w.cfg.MaxPageBytes {
			return nil, fmt.Errorf("page stroke payload exceeds %d bytes", w.cfg.MaxPageBytes)
		}
		raw, err := base64.StdEncoding.DecodeString(stroke.PointsData)
		if err != nil {
			return nil, fmt.Errorf("decoding stroke %d: %w", i, err)
		}
		points, err := sb.Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("decoding stroke %d: %w", i, err)
		}
		if len(points) == 0 {
			continue
		}
		strokes = append(strokes, render.Stroke{Points: points, Size: stroke.Size, Pen: stroke.Pen})
	}
	return strokes, nil
}

// read hands an already-rendered page to the model.
//
// The rendering happens in handlePage rather than here so that the caller keeps the PNG: it is
// the same image a reader wants beside the text, and rendering it twice to have it in both
// places would be the most expensive way to avoid passing an argument.
func (w *Worker) read(ctx context.Context, image render.Result, page Page) (recognize.Result, error) {
	if image.Empty {
		// Recorded as recognized-and-empty rather than skipped, so the page stops being
		// revisited on every pass and readers can tell "blank" from "not looked at yet".
		result := recognize.Result{Text: "", Engine: w.recognizer.Engine()}
		if formatted, ok := w.recognizer.(interface{ Format() string }); ok {
			result.Format = formatted.Format()
		}
		return result, nil
	}

	started := time.Now()
	result, err := w.recognizer.Recognize(ctx, image.PNG)
	if err != nil {
		return recognize.Result{}, fmt.Errorf("recognizing: %w", err)
	}
	w.log.Info("read a page",
		"page", page.ID,
		"pixels", fmt.Sprintf("%dx%d", image.Width, image.Height),
		"chars", len(result.Text),
		"took", time.Since(started).Round(time.Millisecond))
	return result, nil
}

// backfillImage retries missing or outdated ink without calling the model.
func (w *Worker) backfillImage(ctx context.Context, page Page, pageID string) error {
	if w.cfg.ImagesDatabase == "" {
		return nil
	}
	var existing PageImage
	err := w.couch.Get(ctx, w.cfg.ImagesDatabase, pageImageID(pageID), &existing)
	switch {
	case err == nil && existing.SourceFingerprint == inkFingerprint(page):
		return nil
	case err != nil && !errors.Is(err, couch.ErrNotFound):
		return fmt.Errorf("reading page image: %w", err)
	}
	strokes, err := w.decode(page)
	if err != nil {
		return err
	}
	image, err := render.Render(strokes, render.DefaultOptions(page.PageWidth, page.PageHeight))
	if err != nil {
		return fmt.Errorf("rendering page image: %w", err)
	}
	return w.publishImage(ctx, image, page, pageID, page.UpdatedAt)
}

// publishImage errors keep the durable job pending while its successful text remains available.
func (w *Worker) publishImage(ctx context.Context, image render.Result, page Page, pageID, clock string) error {
	if w.cfg.ImagesDatabase == "" {
		return nil
	}
	// Replace erased ink with a blank image rather than leaving the previous writing visible.
	if image.Empty {
		blank := imagepkg.NewRGBA(imagepkg.Rect(0, 0, 1, 1))
		blank.Set(0, 0, color.White)
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, blank); err != nil {
			return err
		}
		image = render.Result{PNG: encoded.Bytes(), Width: 1, Height: 1}
	}
	id := pageImageID(pageID)
	var existing PageImage
	if err := w.couch.Get(ctx, w.cfg.ImagesDatabase, id, &existing); err != nil && !errors.Is(err, couch.ErrNotFound) {
		return fmt.Errorf("reading page image: %w", err)
	}
	fingerprint := inkFingerprint(page)
	if err := w.validateSource(ctx, PageText{PageID: pageID, NotebookID: page.NotebookID, SourceRev: page.Rev, SourceFingerprint: fingerprint}); err != nil {
		return err
	}
	status, err := w.couch.Put(ctx, w.cfg.ImagesDatabase, id, PageImage{
		ID: id, Rev: existing.Rev, PageID: pageID, NotebookID: page.NotebookID,
		Width: image.Width, Height: image.Height, RenderedClock: clock,
		SourceRev: page.Rev, SourceFingerprint: fingerprint, UpdatedAt: nowStamp(), UpdatedBy: w.cfg.DeviceID,
		Attachments: map[string]Attachment{ImageAttachmentName: {ContentType: "image/png", Data: base64.StdEncoding.EncodeToString(image.PNG)}},
	})
	if err != nil {
		return fmt.Errorf("publishing page image: %w", err)
	}
	if status >= 400 {
		return fmt.Errorf("publishing page image: status %d", status)
	}
	return nil
}

// publish writes the text, re-checking the guard if someone got there first.
func (w *Worker) publish(ctx context.Context, text PageText) error {
	if err := w.validateSource(ctx, text); err != nil {
		return err
	}
	status, err := w.couch.Put(ctx, w.cfg.TextDatabase, text.ID, text)
	if err != nil {
		return err
	}
	if status < 400 {
		return nil
	}
	if status != 409 {
		return fmt.Errorf("publishing %s: status %d", text.ID, status)
	}

	// Another writer won the revision. Validate the canonical source again before retrying.
	// Readers reject records whose sourceRev no longer matches their page snapshot.
	var current PageText
	if err := w.couch.Get(ctx, w.cfg.TextDatabase, text.ID, &current); err != nil {
		return err
	}
	if current.SourceRev == text.SourceRev && current.SourceFingerprint == text.SourceFingerprint && current.PipelineVersion == text.PipelineVersion && current.Text == text.Text && current.Format == text.Format && current.PageTitle == text.PageTitle {
		return nil
	}
	if text.SourceFingerprint == "" && !supersedes(text, current) {
		return nil
	}
	if err := w.validateSource(ctx, text); err != nil {
		return err
	}
	text.Rev = current.Rev
	status, err = w.couch.Put(ctx, w.cfg.TextDatabase, text.ID, text)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("publishing %s after conflict: status %d", text.ID, status)
	}
	return nil
}

// Readers also compare sourceRev: the source and text databases cannot be updated atomically.
func (w *Worker) validateSource(ctx context.Context, text PageText) error {
	var page Page
	if err := w.couch.Get(ctx, w.cfg.NotesDatabase, "page:"+text.PageID, &page); err != nil {
		if errors.Is(err, couch.ErrNotFound) {
			return errSourceChanged
		}
		return err
	}
	live, err := w.pageIsLive(ctx, page)
	if err != nil {
		return err
	}
	if !live {
		return errContextUnavailable
	}
	if page.Rev != text.SourceRev || inkFingerprint(page) != text.SourceFingerprint || page.NotebookID != text.NotebookID {
		return errSourceChanged
	}
	return nil
}

// A late result must not resurrect removed pages or notes hidden by Trash.
func (w *Worker) pageIsLive(ctx context.Context, page Page) (bool, error) {
	var notebook struct {
		PageIDs        []string `json:"pageIds"`
		DeletedAt      *string  `json:"deletedAt"`
		ParentFolderID string   `json:"parentFolderId"`
	}
	if err := w.couch.Get(ctx, w.cfg.NotesDatabase, "notebook:"+page.NotebookID, &notebook); err != nil {
		if errors.Is(err, couch.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if notebook.DeletedAt != nil && *notebook.DeletedAt != "" {
		return false, nil
	}
	found := false
	for _, id := range notebook.PageIDs {
		if id == bareID(page.ID) {
			found = true
			break
		}
	}
	if !found {
		return false, nil
	}
	seen := map[string]bool{}
	for id := notebook.ParentFolderID; id != ""; {
		if seen[id] {
			return false, fmt.Errorf("folder cycle")
		}
		seen[id] = true
		var folder struct {
			DeletedAt      *string `json:"deletedAt"`
			ParentFolderID string  `json:"parentFolderId"`
		}
		if err := w.couch.Get(ctx, w.cfg.NotesDatabase, "folder:"+id, &folder); err != nil {
			if errors.Is(err, couch.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		if folder.DeletedAt != nil && *folder.DeletedAt != "" {
			return false, nil
		}
		id = folder.ParentFolderID
	}
	return true, nil
}
