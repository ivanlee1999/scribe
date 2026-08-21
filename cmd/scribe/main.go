// Command scribe reads handwriting out of a Notable library and publishes it as text.
//
// It follows the change feed of the CouchDB the tablets sync with, renders the ink of any page
// whose text has fallen behind, hands the image to a local model, and writes the result to the
// text database. The Obsidian plugin turns that into notes.
//
// It never writes to the library. The notebooks and the ink belong to the tablets' sync
// protocol, and nothing here is entitled to touch them.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ivanlee1999/scribe/internal/config"
	"github.com/ivanlee1999/scribe/internal/couch"
	"github.com/ivanlee1999/scribe/internal/recognize"
	"github.com/ivanlee1999/scribe/internal/worker"
)

func main() {
	once := flag.Bool("once", false, "take one batch of changes and exit")
	check := flag.Bool("check", false, "verify the databases and the model, then exit")
	flag.Parse()

	if err := run(*once, *check); err != nil {
		fmt.Fprintln(os.Stderr, "scribe:", err)
		os.Exit(1)
	}
}

func run(once, check bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("SCRIBE_LOG_LEVEL: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	client := couch.New(cfg.CouchURL, cfg.CouchUser, cfg.CouchPassword)
	// A cascade with no strong model behaves exactly like the single model, so the wiring is the
	// same either way and SCRIBE_MODEL_STRONG stays genuinely optional.
	var strong recognize.Recognizer
	if cfg.ModelStrong != "" {
		strong = recognize.NewVisionModel(
			cfg.ModelURL, cfg.ModelKey, cfg.ModelStrong, cfg.ModelPrompt, cfg.ModelLanguage, cfg.ModelTimeout)
	}
	model := &recognize.Cascade{
		First: recognize.NewVisionModel(
			cfg.ModelURL, cfg.ModelKey, cfg.ModelName, cfg.ModelPrompt, cfg.ModelLanguage, cfg.ModelTimeout),
		Strong: strong,
		Log:    log,
	}

	// Cancelled on SIGINT/SIGTERM so a container stop finishes the page in hand rather than
	// killing it half-published.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if check {
		return preflight(ctx, client, cfg, log)
	}

	log.Info("starting", cfg.Redacted()...)
	if err := preflight(ctx, client, cfg, log); err != nil {
		return err
	}

	w := worker.New(client, model, worker.NewFileStore(cfg.StatePath), worker.Config{
		NotesDatabase: cfg.NotesDatabase,
		TextDatabase:  cfg.TextDatabase,
		DeviceID:      cfg.DeviceID,
		Poll:          cfg.Poll,
		Batch:         cfg.Batch,
	}, log)

	if once {
		_, err := w.Once(ctx)
		return err
	}

	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	log.Info("stopped")
	return nil
}

// preflight checks the two things that are always wrong first: credentials, and whether the
// databases exist. Failing here with a clear message beats a change feed that silently returns
// nothing for a week.
func preflight(ctx context.Context, client *couch.Client, cfg config.Config, log *slog.Logger) error {
	for _, database := range []string{cfg.NotesDatabase, cfg.TextDatabase} {
		if err := client.Ping(ctx, database); err != nil {
			return fmt.Errorf("cannot reach database %q: %w", database, err)
		}
	}
	log.Info("both databases are reachable", "notes", cfg.NotesDatabase, "text", cfg.TextDatabase)
	return nil
}
