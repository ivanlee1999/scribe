package recognize

import (
	"context"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Cascade reads with one model and re-reads with a stronger one when the first answer carries a
// sign of having gone wrong.
//
// The point is that the two failure modes worth paying to avoid are both visible in the answer
// itself, without knowing what the page said. A small model handed handwriting it cannot read
// does not return nothing — it returns fluent, confident, invented text, and that is far worse
// in a vault than a gap, because nothing about it looks like an error afterwards.
//
// Most pages never escalate, so the bill stays near the cheap model's while the pages that
// actually need the strong one get it.
type Cascade struct {
	// First reads every page.
	First Recognizer
	// Strong re-reads the ones First appears to have fumbled. A nil Strong disables escalation,
	// which makes a Cascade with one model behave exactly like that model.
	Strong Recognizer
	// Log records each escalation and why. Optional.
	Log *slog.Logger
}

// Engine names the model that reads by default. The Result of an escalated page carries the
// strong model's name instead, so a document always records what actually read it.
func (c *Cascade) Engine() string { return c.First.Engine() }

func (c *Cascade) Recognize(ctx context.Context, png []byte) (Result, error) {
	first, err := c.First.Recognize(ctx, png)
	if err != nil || c.Strong == nil {
		return first, err
	}

	reason := suspect(first.Text)
	if reason == "" {
		return first, nil
	}

	strong, strongErr := c.Strong.Recognize(ctx, png)
	if strongErr != nil {
		// The cheap answer is worth more than nothing, and a page that stays stale is re-read
		// next pass anyway. Escalation failing must not cost the page.
		if c.Log != nil {
			c.Log.Warn("escalation failed, keeping the first reading",
				"reason", reason, "from", c.First.Engine(), "to", c.Strong.Engine(), "error", strongErr)
		}
		return first, nil
	}
	if c.Log != nil {
		c.Log.Info("escalated", "reason", reason, "from", c.First.Engine(), "to", c.Strong.Engine())
	}
	return strong, nil
}

// suspect reports why a reading looks unreliable, or "" when nothing about it does.
//
// Every rule here comes from an answer a small model actually produced on this library, not
// from what one might imagine going wrong.
func suspect(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		// A blank answer is a page with no writing on it, which is a correct reading and the
		// cheapest possible one. Escalating those would pay the strong model for every doodle.
		return ""
	}
	if hasNonLatinScript(text) {
		// Latin script is where the small models hold up. Handed Chinese, one returned a fluent
		// sentence about a sister and her brother that appears nowhere on the page. French and
		// the other Latin-script languages ride on the cheap model and are caught, if they go
		// wrong, by the two rules below.
		return "non-latin script"
	}
	if hasRunawayRepetition(text) {
		return "repetition"
	}
	if strings.Contains(text, "[?]") {
		// The prompt asks for [?] where a word is illegible, so this is the model reporting its
		// own difficulty. Taking it at its word and paying for a second opinion is the whole idea.
		return "illegible markers"
	}
	return ""
}

// nonLatinScripts are the writing systems whose presence sends a page to the stronger model.
var nonLatinScripts = []*unicode.RangeTable{
	unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul,
	unicode.Cyrillic, unicode.Arabic, unicode.Hebrew, unicode.Thai, unicode.Devanagari,
}

func hasNonLatinScript(text string) bool {
	for _, r := range text {
		if unicode.IsOneOf(nonLatinScripts, r) {
			return true
		}
	}
	return false
}

// hasRunawayRepetition reports whether the text has the shape a decoder takes when it loses the
// thread: one letter over and over, or one short line over and over.
//
// Only letters count toward a run, because a row of dashes is a divider someone drew on purpose.
func hasRunawayRepetition(text string) bool {
	var prev rune
	var run int
	for _, r := range text {
		switch {
		case unicode.IsSpace(r):
			continue
		case !unicode.IsLetter(r):
			prev, run = 0, 0
		case r == prev:
			run++
		default:
			prev, run = r, 1
		}
		if run >= 5 {
			return true
		}
	}

	seen := make(map[string]int)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		// Only short lines: a genuine note can repeat a heading, but not a whole sentence four
		// times over.
		if line == "" || utf8.RuneCountInString(line) > 8 {
			continue
		}
		seen[line]++
		if seen[line] >= 4 {
			return true
		}
	}
	return false
}
