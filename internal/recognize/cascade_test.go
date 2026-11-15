package recognize

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fake struct {
	name string
	text string
	err  error
	runs int
}

func (f *fake) Engine() string { return f.name }
func (f *fake) Recognize(context.Context, []byte) (Result, error) {
	f.runs++
	if f.err != nil {
		return Result{}, f.err
	}
	return Result{Text: f.text, Engine: f.name}, nil
}

func TestCascadeEscalatesOnlyWhenTheFirstReadingLooksWrong(t *testing.T) {
	// The escalating cases are answers a small model actually produced against this library.
	for _, tc := range []struct {
		name, first string
		want        bool
	}{
		{"clean english stays put", "AWS SAA exam\nIAM\nuser, groups, permission", false},
		{"an empty page is a correct reading, not a failure", "", false},
		{"a page of only whitespace", "   \n  ", false},
		{"chinese escalates", "尝试写字看一下\n效果如何", true},
		{"the hallucinated chinese that prompted this", "姐姐陪弟弟去下\n女孩女孩\n玩\n玩\n玩\n玩", true},
		{"a decoding loop escalates", "the answer is aaaaaa", true},
		{"repeated short lines escalate", "w\nw\nw\nw", true},
		{"an illegibility marker escalates", "tell st [?]\n(to st 3)", true},
		{"a divider rule is not repetition", "notes\n-----\nmilk, eggs", false},
		{"french rides on the cheap model", "déjà vu, à côté\nc'est ça", false},
		{"a normal double letter is fine", "coffee, spelling, aardvark", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := &fake{name: "cheap", text: tc.first}
			strong := &fake{name: "strong", text: "STRONG"}
			c := &Cascade{First: first, Strong: strong}

			got, err := c.Recognize(context.Background(), nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			escalated := strong.runs == 1
			if escalated != tc.want {
				t.Errorf("escalated = %v, want %v (reason %q)", escalated, tc.want, suspect(tc.first))
			}
			// A document must record whichever model actually produced its text.
			wantEngine := "cheap"
			if tc.want {
				wantEngine = "strong"
			}
			if got.Engine != wantEngine {
				t.Errorf("Engine = %q, want %q", got.Engine, wantEngine)
			}
		})
	}
}

func TestCascadeKeepsTheFirstReadingWhenEscalationFails(t *testing.T) {
	// A page that stays stale is re-read next pass, so a failed escalation must cost the text
	// it already has rather than the whole page.
	first := &fake{name: "cheap", text: "尝试"}
	strong := &fake{name: "strong", err: errors.New("model is down")}
	c := &Cascade{First: first, Strong: strong}

	got, err := c.Recognize(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Text != "尝试" || got.Engine != "cheap" {
		t.Errorf("got %+v, want the first reading", got)
	}
}

func TestCascadeWithoutAStrongModelIsJustTheOneModel(t *testing.T) {
	first := &fake{name: "cheap", text: "尝试写字"}
	c := &Cascade{First: first}

	got, err := c.Recognize(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Text != "尝试写字" || first.runs != 1 {
		t.Errorf("got %+v after %d runs, want a single plain reading", got, first.runs)
	}
	if c.Engine() != "cheap" {
		t.Errorf("Engine() = %q, want %q", c.Engine(), "cheap")
	}
}

func TestCascadeDoesNotEscalateOnAFirstModelError(t *testing.T) {
	// Nothing was read, so there is nothing to second-guess; the error is the worker's to retry.
	first := &fake{name: "cheap", err: errors.New("timeout")}
	strong := &fake{name: "strong", text: "STRONG"}
	c := &Cascade{First: first, Strong: strong}

	if _, err := c.Recognize(context.Background(), nil); err == nil {
		t.Fatal("want the first model's error to surface")
	}
	if strong.runs != 0 {
		t.Errorf("strong model ran %d times, want 0", strong.runs)
	}
}

func TestCascadePipelineVersionTracksPromptAndStrongModel(t *testing.T) {
	first := NewVisionModel("http://unused", "", "fast", "prompt A", "en", time.Second)
	strong := NewVisionModel("http://unused", "", "strong", "prompt A", "en", time.Second)
	cascade := &Cascade{First: first, Strong: strong}
	before := cascade.PipelineVersion()
	strong.Prompt = "prompt B"
	if before == cascade.PipelineVersion() {
		t.Fatal("strong prompt change did not invalidate pipeline")
	}
	before = cascade.PipelineVersion()
	first.Language = "fr"
	if before == cascade.PipelineVersion() {
		t.Fatal("language change did not invalidate pipeline")
	}
}
