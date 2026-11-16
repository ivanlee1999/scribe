package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ivanlee1999/scribe/internal/render"
	"github.com/ivanlee1999/scribe/internal/sb"
)

const notebookID = "11111111-1111-4111-8111-111111111114"
const pageID = "22222222-2222-4222-8222-222222222214"
const blockID = "33333333-3333-4333-8333-333333333314"
const writerID = "44444444-4444-4444-8444-444444444414"
const generationID = "55555555-5555-4555-8555-555555555514"
const typedParagraph = "Typed paragraph stays exactly as written — no transcription replaces this."
const createdAt = "2026-11-14T12:00:00.000Z"

type document = map[string]any

type phase struct {
	Layout         *layoutExpectation `json:"layout,omitempty"`
	Name           string             `json:"name"`
	ExpectedWords  []string           `json:"expectedWords"`
	ForbiddenWords []string           `json:"forbiddenWords"`
	Page           document           `json:"page"`
}
type manifest struct {
	RequiredFormat      string   `json:"requiredFormat,omitempty"`
	SupersededStrokeIDs []string `json:"supersededStrokeIds"`
	Version             int      `json:"version"`
	NotebookID          string   `json:"notebookId"`
	PageID              string   `json:"pageId"`
	BlockID             string   `json:"blockId"`
	TypedParagraph      string   `json:"typedParagraph"`
	JournalDate         string   `json:"journalDate"`
	ActiveTopics        []string `json:"activeTopics"`
	RemovedTopic        string   `json:"removedTopic"`
	Metadata            document `json:"metadata"`
	Notebook            document `json:"notebook"`
	Organization        document `json:"organization"`
	Phases              []phase  `json:"phases"`
}

type point struct{ x, y float64 }
type paths [][]point

// Deliberately hand-plotted lower-case print: these are real ink paths, not a text raster.
var letters = map[rune]paths{
	'm': {{{0, 1}, {0, .1}, {.1, 0}, {.25, 0}, {.45, .2}, {.45, 1}, {.45, .2}, {.62, 0}, {.8, 0}, {1, .2}, {1, 1}}},
	'i': {{{.25, .05}, {.25, .88}, {.32, 1}, {.6, .96}}, {{.25, -.32}, {.25, -.32}}},
	'l': {{{.25, -.55}, {.25, .9}, {.33, 1}, {.5, .98}}},
	'k': {{{.05, -.55}, {.05, 1}}, {{.85, 0}, {.1, .5}, {.95, 1}}},
	'e': {{{.03, .48}, {.95, .48}, {.9, .2}, {.7, .02}, {.4, 0}, {.13, .17}, {.02, .5}, {.1, .8}, {.35, 1}, {.68, 1}, {.95, .82}}},
	'g': {{{.95, .14}, {.72, 0}, {.35, 0}, {.1, .18}, {.02, .5}, {.1, .82}, {.35, 1}, {.68, 1}, {.95, .76}, {.95, 0}, {.95, 1.35}, {.8, 1.56}, {.5, 1.62}, {.18, 1.45}}},
	's': {{{.94, .14}, {.7, 0}, {.35, .02}, {.1, .2}, {.08, .37}, {.25, .5}, {.72, .59}, {.92, .75}, {.88, .9}, {.63, 1.02}, {.3, 1}, {.04, .85}}},
	'b': {{{.06, -.55}, {.06, 1}, {.06, .25}, {.28, .03}, {.58, 0}, {.85, .2}, {.98, .5}, {.86, .82}, {.58, 1}, {.28, .98}, {.06, .78}}},
	'r': {{{.05, 1}, {.05, 0}, {.05, .34}, {.25, .08}, {.48, 0}, {.75, .05}}},
	'a': {{{.93, .17}, {.68, 0}, {.34, 0}, {.08, .22}, {.02, .58}, {.14, .85}, {.4, 1}, {.69, .96}, {.93, .72}, {.93, 0}, {.93, .91}, {1, 1}}},
	'd': {{{.93, .17}, {.68, 0}, {.34, 0}, {.08, .22}, {.02, .58}, {.14, .85}, {.4, 1}, {.69, .96}, {.93, .72}, {.93, -.55}, {.93, 1}}},
	't': {{{.43, -.5}, {.43, .85}, {.55, 1}, {.8, .95}}, {{.02, .08}, {.9, .08}}},
}

func stableID(name string) string { return namedID("notable-llm-e2e:v2:", name) }
func namedID(namespace, name string) string {
	hash := sha256.Sum256([]byte(namespace + name))
	hash[6] = (hash[6] & 15) | 0x50
	hash[8] = (hash[8] & 63) | 0x80
	b := hash[:16]
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func polyline(values []float64) []byte {
	var out []byte
	var previous int64
	for _, v := range values {
		next := int64(math.Round(v * 100))
		delta := next - previous
		previous = next
		encoded := delta << 1
		if delta < 0 {
			encoded = ^encoded
		}
		for encoded >= 32 {
			out = append(out, byte((encoded&31)|32)+63)
			encoded >>= 5
		}
		out = append(out, byte(encoded)+63)
	}
	return out
}
func encodePoints(points []point) []byte {
	xs, ys := make([]float64, len(points)), make([]float64, len(points))
	for i, p := range points {
		xs[i], ys[i] = p.x, p.y
	}
	var body bytes.Buffer
	for _, axis := range [][]byte{polyline(xs), polyline(ys)} {
		_ = binary.Write(&body, binary.LittleEndian, int32(len(axis)))
		body.Write(axis)
	}
	var out bytes.Buffer
	out.Write([]byte{'S', 'B', 2, 0})
	_ = binary.Write(&out, binary.LittleEndian, int32(len(points)))
	out.WriteByte(0)
	out.Write(body.Bytes())
	return out.Bytes()
}
func wordStrokes(word string, y float64) []document {
	var out []document
	x := 120.0
	for index, letter := range word {
		width := 54.0
		if letter == 'm' {
			width = 76
		}
		if letter == 'i' || letter == 'l' {
			width = 23
		}
		for pathIndex, path := range letters[letter] {
			points := make([]point, len(path))
			left, right, top, bottom := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
			for i, p := range path {
				points[i] = point{x + p.x*width, y + p.y*66}
				left = math.Min(left, points[i].x)
				right = math.Max(right, points[i].x)
				top = math.Min(top, points[i].y)
				bottom = math.Max(bottom, points[i].y)
			}
			out = append(out, document{"id": stableID(fmt.Sprintf("%s/%d/%d", word, index, pathIndex)), "createdAt": createdAt, "updatedAt": createdAt, "deviceId": writerID,
				"pen": "BALLPEN", "color": -16777216, "size": 5, "maxPressure": 1, "left": left, "right": right, "top": top, "bottom": bottom, "pointsData": base64.StdEncoding.EncodeToString(encodePoints(points))})
		}
		x += width + 6
	}
	return out
}
func fixtures() manifest {
	at, _ := time.Parse(time.RFC3339Nano, createdAt)
	m := manifest{Version: 2, NotebookID: notebookID, PageID: pageID, BlockID: blockID, TypedParagraph: typedParagraph, JournalDate: "2026-11-14", ActiveTopics: []string{"groceries", "test"}, RemovedTopic: "removed"}
	m.Metadata = document{"_id": "sync-meta:database", "type": "sync-database-metadata", "protocolVersion": 1, "minimumClientProtocol": 1, "generation": generationID, "locked": false, "updatedAt": createdAt}
	m.Notebook = document{"_id": "notebook:" + notebookID, "type": "notebook", "schema": 1, "title": "Synthetic LLM handwriting verification", "pageIds": []string{pageID}, "deletedPageIds": []document{}, "parentFolderId": nil, "deletedAt": nil, "bookmarks": []document{}, "outline": []document{}, "defaultBackground": "blank", "defaultBackgroundType": "native", "defaultPageWidth": 1080, "defaultPageHeight": 720, "createdAt": createdAt, "updatedAt": createdAt, "updatedBy": writerID}
	m.Organization = document{"_id": "journal:" + notebookID, "type": "journal", "schema": 1, "notebookId": notebookID, "date": m.JournalDate, "createdAt": at.UnixMilli(), "topics": document{
		"groceries": document{"name": "groceries", "present": true, "counter": 1, "writer": writerID}, "test": document{"name": "test", "present": true, "counter": 2, "writer": writerID}, "removed": document{"name": "removed", "present": false, "counter": 3, "writer": writerID}}}
	milk, eggs, bread, tea := wordStrokes("milk", 150), wordStrokes("eggs", 340), wordStrokes("bread", 150), wordStrokes("tea", 150)
	snapshots := [][]document{milk, append(append([]document{}, milk...), eggs...), append(append([]document{}, bread...), eggs...), bread, {}, tea}
	for _, strokes := range snapshots {
		sort.Slice(strokes, func(i, j int) bool { return strokes[i]["id"].(string) < strokes[j]["id"].(string) })
	}
	names := []string{"01-milk", "02-milk-eggs", "03-bread-eggs", "04-bread", "05-blank", "06-tea"}
	expected := [][]string{{"milk"}, {"milk", "eggs"}, {"bread", "eggs"}, {"bread"}, {}, {"tea"}}
	deleted := []document{}
	// Version1 milk remains removed forever, even on a client that cached its old vectors.
	for index, letter := range "milk" {
		for pathIndex := range letters[letter] {
			id := namedID("notable-llm-e2e:", fmt.Sprintf("milk/%d/%d", index, pathIndex))
			m.SupersededStrokeIDs = append(m.SupersededStrokeIDs, id)
			deleted = append(deleted, document{"id": id, "deletedAt": createdAt})
		}
	}
	previous := map[string]bool{}
	for index, strokes := range snapshots {
		clock := at.Add(time.Duration(index) * time.Second).Format("2006-01-02T15:04:05.000Z")
		active := map[string]bool{}
		for _, stroke := range strokes {
			active[stroke["id"].(string)] = true
		}
		// Iterate previous phase order for stable tombstone ordering.
		if index > 0 {
			for _, stroke := range snapshots[index-1] {
				id := stroke["id"].(string)
				if previous[id] && !active[id] {
					deleted = append(deleted, document{"id": id, "deletedAt": clock})
				}
			}
		}
		page := document{"_id": "page:" + pageID, "type": "page", "schema": 1, "notebookId": notebookID, "title": "Synthetic groceries", "background": "blank", "backgroundType": "native", "pageWidth": 1080, "pageHeight": 720, "layout": "sheet", "strokes": strokes, "deletedStrokes": append([]document{}, deleted...), "images": []document{}, "deletedImages": []document{}, "blocks": []document{{"id": blockID, "kind": "md", "orderKey": "a0", "text": typedParagraph, "x": nil, "y": nil, "createdAt": createdAt, "updatedAt": createdAt, "deviceId": writerID}}, "deletedBlocks": []document{}, "createdAt": createdAt, "updatedAt": clock, "updatedBy": writerID}
		forbidden := []string{}
		for _, word := range []string{"milk", "eggs", "bread", "tea"} {
			found := false
			for _, want := range expected[index] {
				if word == want {
					found = true
				}
			}
			if !found {
				forbidden = append(forbidden, word)
			}
		}
		m.Phases = append(m.Phases, phase{Name: names[index], ExpectedWords: expected[index], ForbiddenWords: forbidden, Page: page})
		previous = active
	}
	return m
}
func writeFixtures(dir string, m manifest) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "manifest.json"), append(data, '\n'), 0644); err != nil {
		return err
	}
	for _, phase := range m.Phases {
		pageJSON, _ := json.MarshalIndent(phase.Page, "", "  ")
		if err = os.WriteFile(filepath.Join(dir, phase.Name+".json"), append(pageJSON, '\n'), 0644); err != nil {
			return err
		}
		source, decodeErr := decodedPage(phase.Page)
		if decodeErr != nil {
			return decodeErr
		}
		var strokes []render.Stroke
		for _, stroke := range visibleStrokes(source) {
			raw, decodeErr := base64.StdEncoding.DecodeString(stroke.PointsData)
			if decodeErr != nil {
				return decodeErr
			}
			points, decodeErr := sb.Decode(raw)
			if decodeErr != nil {
				return decodeErr
			}
			strokes = append(strokes, render.Stroke{Points: points, Size: stroke.Size, Pen: stroke.Pen})
		}
		image, renderErr := render.Render(strokes, render.DefaultOptions(source.PageWidth, source.PageHeight))
		if renderErr != nil {
			return renderErr
		}
		if !image.Empty {
			if err = os.WriteFile(filepath.Join(dir, phase.Name+".png"), image.PNG, 0644); err != nil {
				return err
			}
		}
	}
	return nil
}
