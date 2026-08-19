package worker

import "testing"

func text(clock, updated, body, engine string) PageText {
	return PageText{
		RecognizedClock: clock,
		UpdatedAt:       updated,
		Text:            body,
		Engine:          engine,
		Language:        "en",
	}
}

func TestStalenessFollowsTheInk(t *testing.T) {
	if stale("2026-08-18T04:00:00.000Z", "2026-08-18T04:00:00.000Z") {
		t.Error("text read from exactly this ink counts as stale")
	}
	if !stale("2026-08-18T05:00:00.000Z", "2026-08-18T04:00:00.000Z") {
		t.Error("a page edited after it was read is not stale")
	}
	if stale("2026-08-18T03:00:00.000Z", "2026-08-18T04:00:00.000Z") {
		t.Error("text newer than its page counts as stale")
	}
}

func TestStampsWithoutFractionalSecondsAreUnderstood(t *testing.T) {
	// Notable prints fractional seconds only when non-zero, matching java.time.Instant, so both
	// spellings arrive. Failing to parse one would make its page stale forever and recognized on
	// every single pass — a model run per page per poll, for nothing.
	if stale("2026-08-18T04:00:00Z", "2026-08-18T04:00:00.000Z") {
		t.Error("the two spellings of the same instant disagree")
	}
	if !stale("2026-08-18T04:00:01Z", "2026-08-18T04:00:00.000Z") {
		t.Error("a second later is not later")
	}
}

func TestUnreadableClocksMeanDoTheWork(t *testing.T) {
	// The cost of being wrong one way is a wasted model run; the other way is text that never
	// updates again. So an unreadable clock has to mean "read it".
	if !stale("2026-08-18T04:00:00.000Z", "") {
		t.Error("text with no clock was treated as current")
	}
	if !stale("", "2026-08-18T04:00:00.000Z") {
		t.Error("a page with no clock was treated as unchanged")
	}
	if !stale("yesterday", "the day before") {
		t.Error("nonsense clocks were treated as current")
	}
}

func TestTextDescribingNewerInkWins(t *testing.T) {
	newer := text("2026-08-18T09:00:00.000Z", "2026-08-18T09:00:05.000Z", "milk", "server")
	older := text("2026-08-18T04:00:00.000Z", "2026-08-18T23:00:00.000Z", "milk", "boox")

	// Note the older reading has the *later* run time: newer ink still wins, which is the whole
	// point of keying on the ink's clock rather than on when the model happened to run.
	if !supersedes(newer, older) {
		t.Error("a reading of newer ink lost to a later reading of older ink")
	}
	if supersedes(older, newer) {
		t.Error("a reading of older ink won")
	}
}

func TestALaterReadingOfTheSameInkWins(t *testing.T) {
	first := text("2026-08-18T04:00:00.000Z", "2026-08-18T04:00:05.000Z", "rnilk", "boox")
	second := text("2026-08-18T04:00:00.000Z", "2026-08-18T08:00:00.000Z", "milk", "server")

	if !supersedes(second, first) {
		t.Error("the server's later reading of the same ink was refused")
	}
	if supersedes(first, second) {
		t.Error("the earlier reading won")
	}
}

func TestAnUnchangedReadingIsNotRepublished(t *testing.T) {
	// A write that changes nothing still wakes every reader of the change feed — the plugin
	// included, which would then rewrite the note in the vault.
	stored := text("2026-08-18T04:00:00.000Z", "2026-08-18T04:00:05.000Z", "milk", "server")
	again := text("2026-08-18T04:00:00.000Z", "2026-08-19T04:00:05.000Z", "milk", "server")

	if supersedes(again, stored) {
		t.Error("an identical reading was republished")
	}
}

func TestACorruptDocumentIsNotAnImmovableWinner(t *testing.T) {
	corrupt := text("not a date", "not a date", "garbage", "unknown")
	good := text("2026-08-18T04:00:00.000Z", "2026-08-18T04:00:05.000Z", "milk", "server")

	if !supersedes(good, corrupt) {
		t.Error("a readable document could not replace an unreadable one")
	}
	if supersedes(corrupt, good) {
		t.Error("an unreadable document displaced a readable one")
	}
}

func TestDocumentIdentifiers(t *testing.T) {
	if !isPageID("page:abc") || isPageID("notebook:abc") || isPageID("pagetext:abc") {
		t.Error("page documents are not being told apart from the rest")
	}
	if got := pageTextID("abc"); got != "pagetext:abc" {
		t.Errorf("pageTextID = %q", got)
	}
	if got := bareID("page:abc-123"); got != "abc-123" {
		t.Errorf("bareID = %q", got)
	}
	if got := bareID("nocolon"); got != "nocolon" {
		t.Errorf("bareID = %q", got)
	}
}

func TestTheStampWeWriteIsOneWeCanRead(t *testing.T) {
	// The staleness comparison is between our stamp and the tablets', so ours has to parse with
	// the same reader. A format mismatch here would be invisible until every page looped.
	if _, ok := millis(nowStamp()); !ok {
		t.Fatalf("cannot parse our own stamp: %q", nowStamp())
	}
}
