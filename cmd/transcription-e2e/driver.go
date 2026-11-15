package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/ivanlee1999/scribe/internal/worker"
)

type testDriver struct {
	base, db, user, password string
	http                     *http.Client
}

func newDriver(confirm bool) (*testDriver, error) {
	if !confirm {
		return nil, errors.New("network actions require -confirm-isolated")
	}
	base := os.Getenv("SCRIBE_E2E_URL")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("SCRIBE_E2E_URL must be a plain loopback HTTP origin")
	}
	if host := parsed.Hostname(); host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return nil, errors.New("only loopback test endpoints are allowed")
	}
	db := os.Getenv("SCRIBE_E2E_DATABASE")
	if !regexp.MustCompile(`^llm_e2e_[a-z0-9_]+$`).MatchString(db) {
		return nil, errors.New("database must be an explicit llm_e2e_* disposable database")
	}
	user, password := os.Getenv("SCRIBE_E2E_USER"), os.Getenv("SCRIBE_E2E_PASSWORD")
	if user != "llm_e2e" || password == "" {
		return nil, errors.New("explicit llm_e2e test credentials are required")
	}
	return &testDriver{base: strings.TrimRight(base, "/"), db: db, user: user, password: password, http: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (d *testDriver) request(ctx context.Context, method, database, id string, body any) (document, int, error) {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
	}
	path := d.base + "/" + url.PathEscape(database)
	if id != "" {
		path += "/" + url.PathEscape(id)
	}
	req, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(encoded))
	if err != nil {
		return nil, 0, err
	}
	req.SetBasicAuth(d.user, d.password)
	req.Header.Set("Content-Type", "application/json")
	response, err := d.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, response.StatusCode, err
	}
	var result document
	if len(data) > 0 {
		if err = json.Unmarshal(data, &result); err != nil {
			return nil, response.StatusCode, fmt.Errorf("invalid test endpoint JSON (status %d)", response.StatusCode)
		}
	}
	if response.StatusCode >= 400 {
		return result, response.StatusCode, fmt.Errorf("test endpoint %s returned HTTP %d", method, response.StatusCode)
	}
	return result, response.StatusCode, nil
}
func (d *testDriver) get(ctx context.Context, database, id string) (document, error) {
	doc, _, err := d.request(ctx, "GET", database, id, nil)
	return doc, err
}
func (d *testDriver) put(ctx context.Context, database string, doc document) error {
	_, _, err := d.request(ctx, "PUT", database, doc["_id"].(string), doc)
	return err
}
func (d *testDriver) guard(ctx context.Context) error {
	meta, err := d.get(ctx, d.db, "sync-meta:database")
	if err != nil {
		return err
	}
	if meta["generation"] != generationID || meta["type"] != "sync-database-metadata" || meta["locked"] != false {
		return errors.New("test database generation marker does not match; refusing access")
	}
	return nil
}
func (d *testDriver) seed(ctx context.Context, m manifest) error {
	// Never adopt an existing unmarked database, even if its name resembles a test database.
	for _, database := range []string{d.db, d.db + "_organization"} {
		info, err := d.get(ctx, database, "")
		if err != nil {
			return err
		}
		if count, ok := info["doc_count"].(float64); !ok || count != 0 {
			return errors.New("seed requires empty test notes/organization databases")
		}
	}
	if err := d.put(ctx, d.db, m.Metadata); err != nil {
		return err
	}
	if err := d.guard(ctx); err != nil {
		return err
	}
	for _, item := range []struct {
		database string
		doc      document
	}{{d.db + "_organization", m.Organization}, {d.db, m.Notebook}, {d.db, m.Phases[0].Page}} {
		if err := d.put(ctx, item.database, item.doc); err != nil {
			return err
		}
	}
	return nil
}
func typedText(page document) (string, error) {
	blocks, ok := page["blocks"].([]any)
	if !ok {
		return "", errors.New("typed blocks missing")
	}
	removed, _ := page["deletedBlocks"].([]any)
	for _, raw := range removed {
		if block, ok := raw.(map[string]any); ok && block["id"] == blockID {
			return "", errors.New("typed paragraph was tombstoned")
		}
	}
	for _, raw := range blocks {
		if block, ok := raw.(map[string]any); ok && block["id"] == blockID && block["kind"] == "md" {
			text, _ := block["text"].(string)
			return text, nil
		}
	}
	return "", errors.New("typed paragraph missing")
}
func (d *testDriver) preserved(ctx context.Context, m manifest) (document, error) {
	if err := d.guard(ctx); err != nil {
		return nil, err
	}
	notebook, err := d.get(ctx, d.db, "notebook:"+notebookID)
	if err != nil {
		return nil, err
	}
	ids, _ := notebook["pageIds"].([]any)
	found := false
	for _, id := range ids {
		if id == pageID {
			found = true
		}
	}
	if !found || notebook["_deleted"] == true || notebook["deletedAt"] != nil {
		return nil, errors.New("fixture notebook membership/visibility changed")
	}
	page, err := d.get(ctx, d.db, "page:"+pageID)
	if err != nil {
		return nil, err
	}
	text, err := typedText(page)
	if err != nil {
		return nil, err
	}
	if text != typedParagraph {
		return nil, errors.New("typed paragraph changed")
	}
	organization, err := d.get(ctx, d.db+"_organization", "journal:"+notebookID)
	if err != nil {
		return nil, err
	}
	// Ignore only Couch revision; dates, topic counters and manual-removal tombstones must survive exactly.
	delete(organization, "_rev")
	expectedBytes, _ := json.Marshal(m.Organization)
	var expected document
	_ = json.Unmarshal(expectedBytes, &expected)
	if !reflect.DeepEqual(organization, expected) {
		return nil, errors.New("journal date or topic membership/tombstone changed")
	}
	return page, nil
}
func decodedPage(doc document) (worker.Page, error) {
	data, err := json.Marshal(doc)
	if err != nil {
		return worker.Page{}, err
	}
	var page worker.Page
	err = json.Unmarshal(data, &page)
	return page, err
}
func visibleStrokes(page worker.Page) []worker.Stroke {
	removed := map[string]bool{}
	for _, entry := range page.DeletedStrokes {
		removed[entry.ID] = true
	}
	result := []worker.Stroke{}
	for _, stroke := range page.Strokes {
		if !removed[stroke.ID] {
			result = append(result, stroke)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt != result[j].CreatedAt {
			return result[i].CreatedAt < result[j].CreatedAt
		}
		return result[i].ID < result[j].ID
	})
	return result
}
func sourceFingerprint(page worker.Page) string {
	source := struct {
		Width, Height float32
		Strokes       []worker.Stroke
	}{page.PageWidth, page.PageHeight, visibleStrokes(page)}
	data, _ := json.Marshal(source)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}
func matchesInk(actual document, p phase) error {
	current, err := decodedPage(actual)
	if err != nil {
		return err
	}
	expected, err := decodedPage(p.Page)
	if err != nil {
		return err
	}
	if current.NotebookID != notebookID || current.PageWidth != 1080 || current.PageHeight != 720 {
		return errors.New("page identity/dimensions changed")
	}
	a, b := visibleStrokes(current), visibleStrokes(expected)
	// Native authors may stamp their own creation times; identity and ink geometry must match.
	for index := range a {
		a[index].CreatedAt = ""
	}
	for index := range b {
		b[index].CreatedAt = ""
	}
	sort.Slice(a, func(i, j int) bool { return a[i].ID < a[j].ID })
	sort.Slice(b, func(i, j int) bool { return b[i].ID < b[j].ID })
	if !reflect.DeepEqual(a, b) {
		return errors.New("active ink does not match requested phase")
	}
	currentDeleted := map[string]bool{}
	for _, entry := range current.DeletedStrokes {
		currentDeleted[entry.ID] = true
	}
	for _, entry := range expected.DeletedStrokes {
		if !currentDeleted[entry.ID] {
			return errors.New("stroke removal tombstone disappeared")
		}
	}
	return nil
}
func (d *testDriver) apply(ctx context.Context, m manifest, p phase) error {
	current, err := d.preserved(ctx, m)
	if err != nil {
		return err
	}
	page, err := decodedPage(current)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, id := range m.SupersededStrokeIDs {
		known[id] = true
	}
	for _, phase := range m.Phases {
		snapshot, _ := decodedPage(phase.Page)
		for _, stroke := range snapshot.Strokes {
			known[stroke.ID] = true
		}
	}
	for _, stroke := range page.Strokes {
		if !known[stroke.ID] {
			return errors.New("refusing to replace unexpected ink")
		}
	}
	// Preserve every non-ink page field and merge tombstones, including those authored by clients.
	current["strokes"] = p.Page["strokes"]
	tombstones := map[string]document{}
	if entries, ok := current["deletedStrokes"].([]any); ok {
		for _, raw := range entries {
			if entry, ok := raw.(map[string]any); ok {
				tombstones[entry["id"].(string)] = entry
			}
		}
	}
	for _, entry := range p.Page["deletedStrokes"].([]document) {
		id := entry["id"].(string)
		if prior, ok := tombstones[id]; !ok || entry["deletedAt"].(string) < prior["deletedAt"].(string) {
			tombstones[id] = entry
		}
	}
	ids := []string{}
	for id := range tombstones {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	merged := []document{}
	for _, id := range ids {
		merged = append(merged, tombstones[id])
	}
	current["deletedStrokes"] = merged
	current["updatedAt"] = p.Page["updatedAt"]
	current["updatedBy"] = writerID
	return d.put(ctx, d.db, current)
}
func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}
func (d *testDriver) verify(ctx context.Context, m manifest, p phase) (document, error) {
	page, err := d.preserved(ctx, m)
	if err != nil {
		return nil, err
	}
	if err = matchesInk(page, p); err != nil {
		return nil, err
	}
	source, err := decodedPage(page)
	if err != nil {
		return nil, err
	}
	text, err := d.get(ctx, d.db+"_text", "pagetext:"+pageID)
	if err != nil {
		return nil, err
	}
	if text["sourceRev"] != page["_rev"] || text["sourceFingerprint"] != sourceFingerprint(source) {
		return nil, errors.New("transcript is not bound to current canonical ink")
	}
	pipeline, _ := text["pipelineVersion"].(string)
	if !strings.HasPrefix(pipeline, "scribe-v2:") {
		return nil, errors.New("transcript pipeline version missing")
	}
	recognized, _ := text["text"].(string)
	if !slices.Equal(words(recognized), p.ExpectedWords) {
		return nil, fmt.Errorf("actual provider transcript mismatch: got %q, want words %v", recognized, p.ExpectedWords)
	}
	report := document{"phase": p.Name, "pageTitle": page["title"], "sourceRev": page["_rev"], "sourceFingerprint": text["sourceFingerprint"], "pipelineVersion": pipeline, "engine": text["engine"], "text": recognized, "journalDate": m.JournalDate, "activeTopics": m.ActiveTopics, "removedTopic": m.RemovedTopic, "typedParagraph": typedParagraph, "verifiedAt": time.Now().UTC().Format(time.RFC3339Nano)}
	return report, nil
}
func (d *testDriver) wait(ctx context.Context, m manifest, p phase) (document, error) {
	var last error
	for {
		report, err := d.verify(ctx, m, p)
		if err == nil {
			return report, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for %s: %v: %w", p.Name, last, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
func saveReport(dir string, p phase, report document) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, p.Name+"-verified.json"), append(data, '\n'), 0644)
}

// metadata changes only page filing text; it exercises the no-inference update path.
func (d *testDriver) metadata(ctx context.Context, m manifest, p phase) error {
	current, err := d.preserved(ctx, m)
	if err != nil {
		return err
	}
	if err = matchesInk(current, p); err != nil {
		return err
	}
	current["title"] = "Synthetic groceries — renamed"
	current["updatedAt"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	current["updatedBy"] = writerID
	return d.put(ctx, d.db, current)
}
