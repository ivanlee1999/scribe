package recognize

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCleanStripsWhatModelsWrapAroundTranscriptions(t *testing.T) {
	// Asking politely in the prompt gets most of the way. What leaks through goes straight into
	// someone's notes as though they had written it, which is why it is stripped again here.
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain text is untouched", "milk, eggs", "milk, eggs"},
		{"surrounding space goes", "  milk, eggs \n", "milk, eggs"},
		{"a bare fence", "```\nmilk, eggs\n```", "milk, eggs"},
		{"a tagged fence", "```text\nmilk, eggs\n```", "milk, eggs"},
		{"a preamble line", "Here is the transcription:\nmilk, eggs", "milk, eggs"},
		{"a preamble in another shape", "Transcribed text:\nmilk, eggs", "milk, eggs"},
		{"line breaks inside are kept", "milk\neggs\nbread", "milk\neggs\nbread"},
		{"nothing stays nothing", "", ""},
		// The shape models actually produce, and the one an end-to-end run caught getting
		// through: a preamble *and* a fence. Stripping either alone leaves the other behind.
		{
			"a preamble wrapped around a fence",
			"Here is the transcription:\n```\nmilk, eggs\nbread\n```",
			"milk, eggs\nbread",
		},
		{
			"a preamble around a tagged fence",
			"Here's the transcribed text:\n```text\nmilk\n```",
			"milk",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Clean(tc.in); got != tc.want {
				t.Errorf("Clean(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCleanDiscardsCommentaryAboutAnEmptyPage(t *testing.T) {
	// A page of pure drawing gets described rather than transcribed, however plainly the prompt
	// asks for silence. These are verbatim answers from a backfill over a real library.
	for _, in := range []string{
		"There is no handwriting in this image. The image contains only a simple line drawing with curved and wavy lines.",
		"This image contains only line drawings/sketches with no handwritten text to transcribe.",
		"This image contains only a simple line drawing or sketch, not handwriting. There is no text to transcribe.",
		"This image contains only handwritten marks and lines that do not form legible text or words. There is no transcribable handwriting content.",
		"This image contains only handwritten scribbles/marks and no legible text to transcribe.",
		"There is no handwriting in this image. The image contains only a curved line drawing, not text.",
		// A second backfill pass turned up this phrasing, which the first list did not cover.
		"I don't see any legible handwriting text in this image. The image appears to contain only a simple curved line.",
		"I don't see any handwriting in this image. The image contains only a simple black wavy line or squiggle.",
	} {
		t.Run(in[:32], func(t *testing.T) {
			if got := Clean(in); got != "" {
				t.Errorf("Clean(%q) = %q, want %q", in, got, "")
			}
		})
	}
}

func TestCleanKeepsNotesThatMerelyMentionImagesOrText(t *testing.T) {
	// The discard needs both halves and a short block, so that the user's own writing about
	// images, or about missing text, survives.
	for _, tc := range []struct {
		name, in string
	}{
		{"a note about an image with no absence claim", "the image pipeline resizes to 1568px"},
		{"an absence claim with no image", "no text came back from the parser"},
		{"a real note using both words at length", "image formats:\npng is lossless\nno text is stored in exif\njpeg is not\nwebp is smaller\navif is smallest"},
		{"a short note about a missing alt text", "alt text on the image is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Clean(tc.in); got != tc.in {
				t.Errorf("Clean(%q) = %q, want it unchanged", tc.in, got)
			}
		})
	}
}

func TestCleanDoesNotEatTheUsersOwnWriting(t *testing.T) {
	// The stripping has to be conservative in the other direction too. Someone's note that
	// happens to begin with a colon-ended line, or to contain the word transcription, is still
	// their note.
	for _, tc := range []struct {
		name, in string
	}{
		{"a heading that ends in a colon", "Shopping:\nmilk, eggs"},
		{"a note about transcription", "transcription is hard to get right\nespecially cursive"},
		{"a long first line ending in a colon", strings.Repeat("a very long line ", 8) + ":\nmilk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Clean(tc.in); got != strings.TrimSpace(tc.in) {
				t.Errorf("Clean removed the user's own text:\n got %q\nwant %q", got, tc.in)
			}
		})
	}
}

func TestCleanKeepsTheFirstLineOfAnUntaggedFence(t *testing.T) {
	// "```\nmilk, eggs" must not lose the milk to fence-tag stripping.
	if got := Clean("```\nmilk, eggs\nbread\n```"); got != "milk, eggs\nbread" {
		t.Errorf("got %q", got)
	}
}

func TestTheRequestIsShapedTheWayAVisionServerExpects(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("posted to %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("unreadable request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"milk, eggs"}}]}`))
	}))
	defer server.Close()

	model := NewVisionModel(server.URL, "", "test-model", "", "en", 10*time.Second)
	result, err := model.Recognize(context.Background(), []byte("\x89PNG fake"))
	if err != nil {
		t.Fatalf("recognize: %v", err)
	}

	if result.Text != "milk, eggs" {
		t.Errorf("text = %q", result.Text)
	}
	if result.Engine != "test-model" {
		t.Errorf("engine = %q", result.Engine)
	}
	// Temperature zero is not a detail: a model that reworded the same ink on every pass would
	// republish forever, and every republish rewrites a note in the vault.
	if temperature, ok := captured["temperature"].(float64); !ok || temperature != 0 {
		t.Errorf("temperature = %v, want 0", captured["temperature"])
	}

	// The image has to travel as a data URI on a content part, which is the shape every
	// OpenAI-compatible server understands.
	body, _ := json.Marshal(captured)
	if !strings.Contains(string(body), "data:image/png;base64,") {
		t.Errorf("no image in the request: %s", truncate(string(body)))
	}
}

func TestAFailingServerIsReportedRatherThanTreatedAsEmptyText(t *testing.T) {
	// Silently publishing "" for a page the model could not read would look exactly like a
	// blank page, and the page would never be tried again.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	defer server.Close()

	model := NewVisionModel(server.URL, "", "missing", "", "en", 10*time.Second)
	if _, err := model.Recognize(context.Background(), []byte("png")); err == nil {
		t.Error("a 404 from the model was not reported")
	}
}

func TestAnAnswerWithNoChoicesIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	model := NewVisionModel(server.URL, "", "m", "", "en", 10*time.Second)
	if _, err := model.Recognize(context.Background(), []byte("png")); err == nil {
		t.Error("an empty response was accepted as a reading")
	}
}

func TestTheAPIKeyIsSentWhenThereIsOne(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.Write([]byte(`{"choices":[{"message":{"content":"x"}}]}`))
	}))
	defer server.Close()

	model := NewVisionModel(server.URL, "secret", "m", "", "en", 10*time.Second)
	if _, err := model.Recognize(context.Background(), []byte("png")); err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer secret" {
		t.Errorf("Authorization = %q", authorization)
	}
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
