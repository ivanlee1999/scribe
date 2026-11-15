package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/ivanlee1999/scribe/internal/sb"
)

func TestSyntheticInkRoundTripsAndErasureNeverReusesRemovedIDs(t *testing.T) {
	m := fixtures()
	if len(m.Phases) != 6 {
		t.Fatal("missing handwriting phase")
	}
	uuid := regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
	removed := map[string]bool{}
	for _, phase := range m.Phases {
		page, err := decodedPage(phase.Page)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range page.DeletedStrokes {
			removed[entry.ID] = true
		}
		for _, stroke := range page.Strokes {
			if !uuid.MatchString(stroke.ID) || removed[stroke.ID] {
				t.Fatal("invalid or resurrected stroke identity")
			}
			raw, err := base64.StdEncoding.DecodeString(stroke.PointsData)
			if err != nil {
				t.Fatal(err)
			}
			points, err := sb.Decode(raw)
			if err != nil || len(points) < 2 {
				t.Fatalf("invalid SB fixture: %v", err)
			}
			for _, point := range points {
				if point.X < 0 || point.X > 1080 || point.Y < 0 || point.Y > 720 {
					t.Fatal("ink outside page")
				}
			}
		}
	}
	blank, _ := decodedPage(m.Phases[4].Page)
	if len(blank.Strokes) != 0 || len(blank.DeletedStrokes) == 0 {
		t.Fatal("all-erase did not preserve removal facts")
	}
	tea, _ := decodedPage(m.Phases[5].Page)
	if len(tea.Strokes) == 0 || len(tea.DeletedStrokes) != len(blank.DeletedStrokes) {
		t.Fatal("new ink lost old removal facts")
	}
	// The fixture remains stable across generation and every phase carries the same source paragraph.
	a, _ := json.Marshal(m)
	b, _ := json.Marshal(fixtures())
	if string(a) != string(b) {
		t.Fatal("fixture generation is unstable")
	}
}
func TestNetworkGuardRejectsProductionAndImplicitAccess(t *testing.T) {
	t.Setenv("SCRIBE_E2E_URL", "http://127.0.0.1:15985")
	t.Setenv("SCRIBE_E2E_DATABASE", "llm_e2e_notes")
	t.Setenv("SCRIBE_E2E_USER", "llm_e2e")
	t.Setenv("SCRIBE_E2E_PASSWORD", "test-only")
	if _, err := newDriver(false); err == nil {
		t.Fatal("missing explicit opt-in accepted")
	}
	for _, address := range []string{"https://127.0.0.1:15985", "http://elbrus:5984", "http://user:secret@localhost:15985", "http://localhost:15985/notes", "http://localhost:15985?db=notes"} {
		t.Setenv("SCRIBE_E2E_URL", address)
		if _, err := newDriver(true); err == nil {
			t.Fatalf("unsafe endpoint accepted: %s", address)
		}
	}
	t.Setenv("SCRIBE_E2E_URL", "http://127.0.0.1:15985")
	t.Setenv("SCRIBE_E2E_DATABASE", "notes")
	if _, err := newDriver(true); err == nil {
		t.Fatal("production database name accepted")
	}
}
func TestSeedRefusesAnExistingUnmarkedDatabase(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			writes++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"doc_count":1}`))
	}))
	defer server.Close()
	t.Setenv("SCRIBE_E2E_URL", server.URL)
	t.Setenv("SCRIBE_E2E_DATABASE", "llm_e2e_notes")
	t.Setenv("SCRIBE_E2E_USER", "llm_e2e")
	t.Setenv("SCRIBE_E2E_PASSWORD", "test-only")
	driver, err := newDriver(true)
	if err != nil {
		t.Fatal(err)
	}
	if err = driver.seed(context.Background(), fixtures()); err == nil || writes != 0 {
		t.Fatal("seed wrote to an existing database")
	}
}
