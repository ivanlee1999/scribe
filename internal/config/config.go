// Package config reads the service's settings from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	CouchURL      string
	CouchUser     string
	CouchPassword string
	NotesDatabase string
	TextDatabase  string

	ModelURL      string
	ModelKey      string
	ModelName     string
	ModelPrompt   string
	ModelTimeout  time.Duration
	ModelLanguage string

	DeviceID  string
	StatePath string
	Poll      time.Duration
	Batch     int
	LogLevel  string
}

// Load reads the environment, filling in defaults and rejecting a configuration that cannot work.
func Load() (Config, error) {
	cfg := Config{
		CouchURL:      os.Getenv("SCRIBE_COUCH_URL"),
		CouchUser:     os.Getenv("SCRIBE_COUCH_USER"),
		CouchPassword: os.Getenv("SCRIBE_COUCH_PASSWORD"),
		NotesDatabase: envOr("SCRIBE_NOTES_DB", "notes"),
		TextDatabase:  envOr("SCRIBE_TEXT_DB", "notes_text"),

		ModelURL:      envOr("SCRIBE_MODEL_URL", "http://127.0.0.1:11434/v1"),
		ModelKey:      os.Getenv("SCRIBE_MODEL_KEY"),
		ModelName:     os.Getenv("SCRIBE_MODEL"),
		ModelPrompt:   os.Getenv("SCRIBE_PROMPT"),
		ModelLanguage: envOr("SCRIBE_LANGUAGE", "en"),

		DeviceID:  envOr("SCRIBE_DEVICE_ID", "server"),
		StatePath: envOr("SCRIBE_STATE", "/var/lib/scribe/state.json"),
		LogLevel:  envOr("SCRIBE_LOG_LEVEL", "info"),
	}

	var err error
	// A page can take a model on this class of hardware a good while; the default is generous
	// on purpose, because a timeout that fires mid-read costs the whole page.
	if cfg.ModelTimeout, err = envDuration("SCRIBE_MODEL_TIMEOUT", 5*time.Minute); err != nil {
		return cfg, err
	}
	if cfg.Poll, err = envDuration("SCRIBE_POLL", 30*time.Second); err != nil {
		return cfg, err
	}
	if cfg.Batch, err = envInt("SCRIBE_BATCH", 200); err != nil {
		return cfg, err
	}

	if cfg.CouchURL == "" {
		return cfg, fmt.Errorf("SCRIBE_COUCH_URL is required")
	}
	if !strings.HasPrefix(cfg.CouchURL, "http://") && !strings.HasPrefix(cfg.CouchURL, "https://") {
		return cfg, fmt.Errorf("SCRIBE_COUCH_URL must be an http(s) URL")
	}
	if cfg.ModelName == "" {
		return cfg, fmt.Errorf("SCRIBE_MODEL is required (the model name your server exposes)")
	}
	// The tablets write "boox" and "ipad". Sharing an id with one of them would make the two
	// writers indistinguishable in the documents, which is the one thing a reader needs.
	if cfg.DeviceID == "boox" || cfg.DeviceID == "ipad" {
		return cfg, fmt.Errorf("SCRIBE_DEVICE_ID must differ from the tablets' ids")
	}
	return cfg, nil
}

// Redacted is the configuration as it is safe to log — the URL can carry credentials.
func (c Config) Redacted() []any {
	return []any{
		"couch", redactURL(c.CouchURL),
		"notes", c.NotesDatabase,
		"text", c.TextDatabase,
		"model", c.ModelName,
		"modelURL", redactURL(c.ModelURL),
		"deviceID", c.DeviceID,
		"poll", c.Poll,
	}
}

// redactURL removes any userinfo. `https://user:password@host` is an ordinary way to write a
// CouchDB URL, and the whole point of a log is that it gets kept and pasted into bug reports.
func redactURL(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return raw
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		return scheme + "://[redacted]@" + rest[at+1:]
	}
	return raw
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback, fmt.Errorf("%s: %w", name, err)
	}
	return parsed, nil
}

func envInt(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback, fmt.Errorf("%s: %w", name, err)
	}
	return parsed, nil
}
