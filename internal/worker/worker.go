// Package worker is the loop: find pages whose ink has outrun their text, read them, publish.
package worker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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
	// ImagesDatabase holds the rendered ink. Empty disables storing it at all.
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
}

// Store persists the change-feed checkpoint across restarts.
type Store interface {
	Checkpoint() (string, error)
	SetCheckpoint(seq string) error
}

type Worker struct {
	couch      *couch.Client
	recognizer recognize.Recognizer
	store      Store
	cfg        Config
	log        *slog.Logger
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
	return &Worker{couch: client, recognizer: recognizer, store: store, cfg: cfg, log: log}
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
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(w.cfg.Poll):
		}
	}
}

// Once takes one batch of changes, returning whether there may be more.
//
// The checkpoint moves only after the batch is fully handled, so an interruption re-reads rather
// than skips. Re-reading is cheap and idempotent — the staleness check finds nothing to do — and
// skipping would leave a page silently unrecognized forever.
func (w *Worker) Once(ctx context.Context) (bool, error) {
	since, err := w.store.Checkpoint()
	if err != nil {
		return false, fmt.Errorf("reading checkpoint: %w", err)
	}

	last, changes, err := w.couch.Changes(ctx, w.cfg.NotesDatabase, since, w.cfg.Batch)
	if err != nil {
		return false, fmt.Errorf("reading changes: %w", err)
	}
	if len(changes) == 0 {
		return false, nil
	}

	for _, change := range changes {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if !isPageID(change.ID) || change.Deleted {
			// A deleted page's text is left where it is. Nothing reads text for a page it does
			// not have, and the Obsidian plugin — which holds the whole library — prunes it.
			continue
		}
		if err := w.handlePage(ctx, change.ID); err != nil {
			// One bad page must not stop the feed. Its change is behind the checkpoint after
			// this, so it is retried the next time the page is edited rather than immediately —
			// which is the right trade for a failure that is usually about that page's content.
			w.log.Error("page failed", "page", change.ID, "error", err)
		}
	}

	if err := w.store.SetCheckpoint(last); err != nil {
		return false, fmt.Errorf("saving checkpoint: %w", err)
	}
	return len(changes) == w.cfg.Batch, nil
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

	if haveExisting && !stale(page.UpdatedAt, existing.RecognizedClock) {
		// The text is current, but the image may not be: an images database can be turned on
		// long after the text was read, and a failed upload leaves a gap behind. Rendering is
		// local and costs milliseconds; recognizing is neither, so the picture is caught up on
		// its own rather than by paying to read the page a second time.
		w.backfillImage(ctx, page, pageID)
		return nil
	}

	// Read before the ink is decoded, so that ink arriving mid-recognition leaves the page
	// looking stale afterwards and gets picked up next time. Reading it after would stamp that
	// ink as already recognized, and it would never be read at all.
	clock := page.UpdatedAt

	strokes, err := w.decode(page)
	if err != nil {
		return err
	}

	image, err := render.Render(strokes, render.DefaultOptions(page.PageWidth, page.PageHeight))
	if err != nil {
		return fmt.Errorf("rendering: %w", err)
	}

	result, err := w.read(ctx, image, page)
	if err != nil {
		return err
	}

	// Before the text's own supersede check, because the image is ours regardless of whose text
	// wins: a page whose text another writer already described better is still a page someone
	// may want to look at.
	w.publishImage(ctx, image, page, pageID, clock)

	text := PageText{
		ID:              textID,
		Rev:             existing.Rev,
		PageID:          pageID,
		NotebookID:      page.NotebookID,
		PageTitle:       page.Title,
		Text:            result.Text,
		Engine:          result.Engine,
		Language:        result.Language,
		RecognizedClock: clock,
		UpdatedAt:       nowStamp(),
		UpdatedBy:       w.cfg.DeviceID,
	}

	if haveExisting && !supersedes(text, existing) {
		return nil
	}

	return w.publish(ctx, text)
}

// decode turns a page's stroke payloads into geometry.
func (w *Worker) decode(page Page) ([]render.Stroke, error) {
	strokes := make([]render.Stroke, 0, len(page.Strokes))
	for i, stroke := range page.Strokes {
		if len(stroke.PointsData) > w.cfg.MaxPageBytes {
			return nil, fmt.Errorf("stroke %d is %d bytes, over the limit", i, len(stroke.PointsData))
		}
		raw, err := base64.StdEncoding.DecodeString(stroke.PointsData)
		if err != nil {
			// One unreadable stroke is not a reason to abandon the page: the rest of the
			// writing is still worth reading.
			w.log.Warn("skipping unreadable stroke", "page", page.ID, "index", i, "error", err)
			continue
		}
		points, err := sb.Decode(raw)
		if err != nil {
			w.log.Warn("skipping undecodable stroke", "page", page.ID, "index", i, "error", err)
			continue
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
		return recognize.Result{Text: "", Engine: w.recognizer.Engine()}, nil
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

// backfillImage renders and stores a page whose text is already current but whose image is
// missing or older than the ink. It never calls the model.
func (w *Worker) backfillImage(ctx context.Context, page Page, pageID string) {
	if w.cfg.ImagesDatabase == "" {
		return
	}

	var existing PageImage
	err := w.couch.Get(ctx, w.cfg.ImagesDatabase, pageImageID(pageID), &existing)
	switch {
	case err == nil && !stale(page.UpdatedAt, existing.RenderedClock):
		return
	case err != nil && !errors.Is(err, couch.ErrNotFound):
		w.log.Warn("could not read the existing image", "page", page.ID, "error", err)
		return
	}

	strokes, err := w.decode(page)
	if err != nil {
		w.log.Warn("could not decode for an image", "page", page.ID, "error", err)
		return
	}
	image, err := render.Render(strokes, render.DefaultOptions(page.PageWidth, page.PageHeight))
	if err != nil {
		w.log.Warn("could not render an image", "page", page.ID, "error", err)
		return
	}
	w.publishImage(ctx, image, page, pageID, page.UpdatedAt)
}

// publishImage stores the rendered ink beside its text, when an images database is configured.
//
// Deliberately best-effort and never returned as an error: the text is what this service exists
// to produce, and a picture that failed to upload must not cost a page its reading. A page that
// fails here is re-rendered the next time its ink changes, which is the only time the image is
// out of date anyway.
func (w *Worker) publishImage(ctx context.Context, image render.Result, page Page, pageID, clock string) {
	if w.cfg.ImagesDatabase == "" || image.Empty {
		return
	}

	id := pageImageID(pageID)
	var existing PageImage
	if err := w.couch.Get(ctx, w.cfg.ImagesDatabase, id, &existing); err != nil && !errors.Is(err, couch.ErrNotFound) {
		w.log.Warn("could not read the existing image", "page", page.ID, "error", err)
		return
	}

	if _, err := w.couch.Put(ctx, w.cfg.ImagesDatabase, id, PageImage{
		ID:            id,
		Rev:           existing.Rev,
		PageID:        pageID,
		NotebookID:    page.NotebookID,
		Width:         image.Width,
		Height:        image.Height,
		RenderedClock: clock,
		UpdatedAt:     nowStamp(),
		UpdatedBy:     w.cfg.DeviceID,
		Attachments: map[string]Attachment{
			ImageAttachmentName: {ContentType: "image/png", Data: base64.StdEncoding.EncodeToString(image.PNG)},
		},
	}); err != nil {
		w.log.Warn("could not store the image", "page", page.ID, "error", err)
	}
}

// publish writes the text, re-checking the guard if someone got there first.
func (w *Worker) publish(ctx context.Context, text PageText) error {
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

	// A tablet wrote between the read and the write. Re-read and re-apply the guard rather than
	// forcing this copy through: if their text describes newer ink, this server stands down.
	var current PageText
	if err := w.couch.Get(ctx, w.cfg.TextDatabase, text.ID, &current); err != nil {
		return err
	}
	if !supersedes(text, current) {
		return nil
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
