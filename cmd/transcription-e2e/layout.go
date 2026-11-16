package main

import (
	"encoding/base64"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

type taskExpectation struct {
	Text    string `json:"text"`
	Checked bool   `json:"checked"`
}
type tableExpectation struct {
	Header []string   `json:"header"`
	Rows   [][]string `json:"rows"`
}
type layoutExpectation struct {
	OrderedNumbers []string          `json:"orderedNumbers"`
	Headings       []string          `json:"headings"`
	Bullets        []string          `json:"bullets"`
	Ordered        []string          `json:"ordered"`
	Tasks          []taskExpectation `json:"tasks"`
	Table          tableExpectation  `json:"table"`
}

const layoutAt = "2026-11-16T12:00:00.000Z"
const layoutNotebookID = "66666666-6666-4666-8666-666666666616"
const layoutPageID = "77777777-7777-4777-8777-777777777716"
const layoutBlockID = "88888888-8888-4888-8888-888888888816"
const layoutWriterID = "99999999-9999-4999-8999-999999999916"
const layoutGeneration = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa16"

func layoutStroke(name string, points []point, size float64) document {
	left, right, top, bottom := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	for _, p := range points {
		left = math.Min(left, p.x)
		right = math.Max(right, p.x)
		top = math.Min(top, p.y)
		bottom = math.Max(bottom, p.y)
	}
	return document{"id": namedID("notable-layout-e2e:", name), "createdAt": layoutAt, "updatedAt": layoutAt, "deviceId": layoutWriterID, "pen": "BALLPEN", "color": -16777216, "size": size, "maxPressure": 1, "left": left, "right": right, "top": top, "bottom": bottom, "pointsData": base64.StdEncoding.EncodeToString(encodePoints(points))}
}
func layoutWord(name, word string, x, y, scale float64) []document {
	digits := map[rune]paths{
		'1': {{{.1, .25}, {.5, 0}, {.5, 1}}, {{.16, 1}, {.86, 1}}},
		'2': {{{.05, .2}, {.22, .04}, {.5, 0}, {.8, .08}, {.95, .3}, {.88, .48}, {.08, 1}, {.98, 1}}},
		'3': {{{.08, .12}, {.35, 0}, {.72, .02}, {.95, .2}, {.9, .36}, {.65, .5}, {.92, .65}, {.96, .85}, {.72, 1}, {.35, 1}, {.05, .88}}},
	}
	var out []document
	for index, letter := range word {
		width := 54.0
		if letter == 'm' {
			width = 76
		}
		if letter == 'i' || letter == 'l' {
			width = 23
		}
		paths := letters[letter]
		if paths == nil {
			paths = digits[letter]
		}
		for pathIndex, path := range paths {
			points := make([]point, len(path))
			for i, p := range path {
				points[i] = point{x + p.x*width*scale, y + p.y*66*scale}
			}
			out = append(out, layoutStroke(fmt.Sprintf("%s/%d/%d", name, index, pathIndex), points, 4*scale))
		}
		x += (width + 6) * scale
	}
	return out
}
func box(name string, x, y float64) document {
	return layoutStroke(name, []point{{x, y}, {x + 40, y}, {x + 40, y + 40}, {x, y + 40}, {x, y}}, 3)
}
func check(name string, x, y float64) []document {
	return []document{layoutStroke(name+"/a", []point{{x + 7, y + 7}, {x + 33, y + 33}}, 4), layoutStroke(name+"/b", []point{{x + 33, y + 7}, {x + 7, y + 33}}, 4)}
}
func layoutFixtures() manifest {
	at, _ := time.Parse(time.RFC3339Nano, layoutAt)
	m := manifest{Version: 1, RequiredFormat: "markdown", NotebookID: layoutNotebookID, PageID: layoutPageID, BlockID: layoutBlockID, TypedParagraph: typedParagraph, JournalDate: "2026-11-16", ActiveTopics: []string{"groceries", "test"}, RemovedTopic: "removed"}
	m.Metadata = document{"_id": "sync-meta:database", "type": "sync-database-metadata", "protocolVersion": 1, "minimumClientProtocol": 1, "generation": layoutGeneration, "locked": false, "updatedAt": layoutAt}
	m.Notebook = document{"_id": "notebook:" + m.NotebookID, "type": "notebook", "schema": 1, "title": "Synthetic layout verification", "pageIds": []string{m.PageID}, "deletedPageIds": []document{}, "parentFolderId": nil, "deletedAt": nil, "bookmarks": []document{}, "outline": []document{}, "defaultBackground": "blank", "defaultBackgroundType": "native", "defaultPageWidth": 1080, "defaultPageHeight": 1440, "createdAt": layoutAt, "updatedAt": layoutAt, "updatedBy": layoutWriterID}
	m.Organization = document{"_id": "journal:" + m.NotebookID, "type": "journal", "schema": 1, "notebookId": m.NotebookID, "date": m.JournalDate, "createdAt": at.UnixMilli(), "topics": document{"groceries": document{"name": "groceries", "present": true, "counter": 1, "writer": layoutWriterID}, "test": document{"name": "test", "present": true, "counter": 2, "writer": layoutWriterID}, "removed": document{"name": "removed", "present": false, "counter": 3, "writer": layoutWriterID}}}
	strokes := layoutWord("heading", "meal", 100, 140, 1.6)
	strokes = append(strokes, layoutStroke("bullet", []point{{115, 368}, {115, 368}}, 9))
	strokes = append(strokes, layoutWord("bullet-milk", "milk", 160, 340, .9)...)
	strokes = append(strokes, layoutWord("ordered-number", "1", 100, 460, .8)...)
	strokes = append(strokes, layoutStroke("ordered-dot", []point{{145, 511}, {145, 511}}, 5))
	strokes = append(strokes, layoutWord("ordered-bread", "bread", 170, 460, .9)...)
	strokes = append(strokes, box("tea-box", 100, 580))
	strokes = append(strokes, layoutWord("task-tea", "tea", 160, 580, .8)...)
	strokes = append(strokes, box("eggs-box", 100, 700))
	strokes = append(strokes, check("eggs-check", 100, 700)...)
	strokes = append(strokes, layoutWord("task-eggs", "eggs", 160, 700, .8)...)
	for index, y := range []float64{900, 1020, 1140, 1260} {
		strokes = append(strokes, layoutStroke(fmt.Sprintf("table-horizontal/%d", index), []point{{100, y}, {970, y}}, 2.5))
	}
	for index, x := range []float64{100, 670, 970} {
		strokes = append(strokes, layoutStroke(fmt.Sprintf("table-vertical/%d", index), []point{{x, 900}, {x, 1260}}, 2.5))
	}
	for _, entry := range []struct {
		name, text string
		x, y       float64
	}{{"header-item", "item", 125, 930}, {"header-kg", "kg", 720, 930}, {"row-milk", "milk", 125, 1065}, {"qty-milk", "1", 740, 1065}, {"row-bread", "bread", 125, 1185}} {
		strokes = append(strokes, layoutWord(entry.name, entry.text, entry.x, entry.y, .8)...)
	}
	qty2 := layoutWord("qty-bread-2", "2", 740, 1185, .8)
	strokes = append(strokes, qty2...)
	checked := append(append([]document{}, strokes...), check("tea-check", 100, 580)...)
	edited := []document{}
	removedQty := map[string]bool{}
	for _, stroke := range qty2 {
		removedQty[stroke["id"].(string)] = true
	}
	deleted := []document{}
	for _, stroke := range checked {
		if removedQty[stroke["id"].(string)] {
			deleted = append(deleted, document{"id": stroke["id"], "deletedAt": at.Add(2 * time.Second).Format(time.RFC3339Nano)})
		} else {
			edited = append(edited, stroke)
		}
	}
	edited = append(edited, layoutWord("qty-bread-3", "3", 740, 1185, .8)...)
	allDeleted := append([]document{}, deleted...)
	for _, stroke := range edited {
		allDeleted = append(allDeleted, document{"id": stroke["id"], "deletedAt": at.Add(3 * time.Second).Format(time.RFC3339Nano)})
	}
	snapshots := [][]document{strokes, checked, edited, {}}
	deletions := [][]document{{}, {}, deleted, allDeleted}
	names := []string{"01-layout", "02-checked", "03-cell-edited", "04-erased"}
	for index, active := range snapshots {
		sort.Slice(active, func(i, j int) bool { return active[i]["id"].(string) < active[j]["id"].(string) })
		clock := at.Add(time.Duration(index) * time.Second).Format("2006-01-02T15:04:05.000Z")
		p := phase{Name: names[index], ExpectedWords: []string{}, ForbiddenWords: []string{}, Page: document{"_id": "page:" + m.PageID, "type": "page", "schema": 1, "notebookId": m.NotebookID, "title": "Synthetic layout", "background": "blank", "backgroundType": "native", "pageWidth": 1080, "pageHeight": 1440, "layout": "sheet", "strokes": active, "deletedStrokes": deletions[index], "images": []document{}, "deletedImages": []document{}, "blocks": []document{{"id": m.BlockID, "kind": "md", "orderKey": "a0", "text": m.TypedParagraph, "x": nil, "y": nil, "createdAt": layoutAt, "updatedAt": layoutAt, "deviceId": layoutWriterID}}, "deletedBlocks": []document{}, "createdAt": layoutAt, "updatedAt": clock, "updatedBy": layoutWriterID}}
		if index < 3 {
			qty := "2"
			if index == 2 {
				qty = "3"
			}
			p.Layout = &layoutExpectation{Headings: []string{"meal"}, Bullets: []string{"milk"}, Ordered: []string{"bread"}, OrderedNumbers: []string{"1"}, Tasks: []taskExpectation{{"tea", index > 0}, {"eggs", true}}, Table: tableExpectation{Header: []string{"item", "kg"}, Rows: [][]string{{"milk", "1"}, {"bread", qty}}}}
			p.ExpectedWords = []string{"meal", "milk", "bread", "tea", "eggs", "item", "kg", "milk", "1", "bread", qty}
		}
		m.Phases = append(m.Phases, p)
	}
	return m
}

var headingLine = regexp.MustCompile(`^#{1,6}\s+(.+)$`)
var taskLine = regexp.MustCompile(`^[-*+]\s+\[([ xX])\]\s+(.+)$`)
var bulletLine = regexp.MustCompile(`^[-*+]\s+(.+)$`)
var orderedLine = regexp.MustCompile(`^([0-9]+)[.)]\s+(.+)$`)
var tableSeparator = regexp.MustCompile(`^:?-{3,}:?$`)

func checkLayout(text string, p phase) error {
	observed := layoutExpectation{Headings: []string{}, Bullets: []string{}, Ordered: []string{}, OrderedNumbers: []string{}, Tasks: []taskExpectation{}, Table: tableExpectation{Header: []string{}, Rows: [][]string{}}}
	visible := []string{}
	separators := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if match := headingLine.FindStringSubmatch(lower); match != nil {
			observed.Headings = append(observed.Headings, match[1])
			visible = append(visible, match[1])
			continue
		}
		if match := taskLine.FindStringSubmatch(lower); match != nil {
			observed.Tasks = append(observed.Tasks, taskExpectation{match[2], match[1] != " "})
			visible = append(visible, match[2])
			continue
		}
		if match := bulletLine.FindStringSubmatch(lower); match != nil {
			observed.Bullets = append(observed.Bullets, match[1])
			visible = append(visible, match[1])
			continue
		}
		if match := orderedLine.FindStringSubmatch(lower); match != nil {
			observed.OrderedNumbers = append(observed.OrderedNumbers, match[1])
			observed.Ordered = append(observed.Ordered, match[2])
			visible = append(visible, match[2])
			continue
		}
		if strings.Contains(lower, "|") {
			cells := strings.Split(strings.TrimSuffix(strings.TrimPrefix(lower, "|"), "|"), "|")
			separator := true
			for index := range cells {
				cells[index] = strings.TrimSpace(cells[index])
				separator = separator && tableSeparator.MatchString(cells[index])
			}
			if separator {
				if len(observed.Table.Header) == 0 || len(observed.Table.Rows) > 0 {
					return fmt.Errorf("misplaced table separator")
				}
				separators++
				continue
			}
			if len(observed.Table.Header) == 0 {
				observed.Table.Header = cells
			} else {
				observed.Table.Rows = append(observed.Table.Rows, cells)
			}
			visible = append(visible, cells...)
			continue
		}
		return fmt.Errorf("unexpected transcript line %q", line)
	}
	if separators != 1 {
		return fmt.Errorf("expected one GFM table separator")
	}
	if !reflect.DeepEqual(observed, *p.Layout) {
		return fmt.Errorf("layout mismatch: got %+v, want %+v", observed, *p.Layout)
	}
	if !slices.Equal(words(strings.Join(visible, " ")), p.ExpectedWords) {
		return fmt.Errorf("visible word/number mismatch")
	}
	return nil
}
