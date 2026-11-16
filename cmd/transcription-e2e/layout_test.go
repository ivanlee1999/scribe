package main

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/ivanlee1999/scribe/internal/sb"
)

const layoutBaseline = "# meal\n\n- milk\n\n1. bread\n\n- [ ] tea\n- [x] eggs\n\n| item | kg |\n| --- | --- |\n| milk | 1 |\n| bread | 2 |"

func TestLayoutFixtureIsRealInkAndPhasesPreserveRemoveWins(t *testing.T) {
	m := layoutFixtures()
	if m.NotebookID == notebookID || m.Metadata["generation"] == generationID {
		t.Fatal("layout fixture reused old database identity")
	}
	removed := map[string]bool{}
	for _, p := range m.Phases {
		page, err := decodedPage(p.Page)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range page.DeletedStrokes {
			removed[entry.ID] = true
		}
		for _, stroke := range page.Strokes {
			if removed[stroke.ID] {
				t.Fatal("removed ink was reused")
			}
			raw, _ := base64.StdEncoding.DecodeString(stroke.PointsData)
			points, err := sb.Decode(raw)
			if err != nil || len(points) < 2 {
				t.Fatal("invalid canonical SB")
			}
			for _, point := range points {
				if point.X < 0 || point.X > 1080 || point.Y < 0 || point.Y > 1440 {
					t.Fatal("layout ink outside sheet")
				}
			}
		}
	}
	if err := checkLayout(layoutBaseline, m.Phases[0]); err != nil {
		t.Fatal(err)
	}
	checked := strings.Replace(layoutBaseline, "[ ] tea", "[x] tea", 1)
	if err := checkLayout(checked, m.Phases[1]); err != nil {
		t.Fatal(err)
	}
	if err := checkLayout(strings.Replace(checked, "bread | 2", "bread | 3", 1), m.Phases[2]); err != nil {
		t.Fatal(err)
	}
	blank, _ := decodedPage(m.Phases[3].Page)
	if len(blank.Strokes) != 0 || len(blank.DeletedStrokes) == 0 {
		t.Fatal("erase phase lost removal facts")
	}
}
func TestLayoutOracleRejectsChangedStateNumbersAndInventedContent(t *testing.T) {
	baseline := layoutFixtures().Phases[0]
	for _, bad := range []string{
		strings.Replace(layoutBaseline, "[ ] tea", "[x] tea", 1),
		strings.Replace(layoutBaseline, "bread | 2", "bread | 99", 1),
		strings.Replace(layoutBaseline, "1. bread", "2. bread", 1),
		strings.Replace(layoutBaseline, "| --- | --- |\n", "", 1),
		layoutBaseline + "\nextra invented words 99",
	} {
		if err := checkLayout(bad, baseline); err == nil {
			t.Fatalf("accepted incorrect transcript %q", bad)
		}
	}
}
