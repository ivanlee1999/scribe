# scribe

Reads the handwriting in a Notable library and publishes it as text.

It follows the change feed of the CouchDB the tablets sync with, renders the ink of any page
whose text has fallen behind, hands the image to a local model, and writes the result to the
text database. The [Obsidian plugin](../obsidian-notable) turns that into notes.

**It never writes to the library.** The notebooks and the ink belong to the tablets' sync
protocol, and nothing here is entitled to touch them. The only thing scribe writes anywhere is
one `pagetext:` document per page, in a separate database.

```
CouchDB "notes"  ──ink──▶  scribe  ──image──▶  model (HTTP)
                              │
                              └──text──▶  CouchDB "notes_text"  ──▶  Obsidian plugin  ──▶  vault
```

## Where this fits

The two tablets can already read their own handwriting — the BOOX through its firmware's
MyScript engine, the iPad through Apple's Vision framework — and both publish to the same text
database. Scribe is a third writer alongside them, and they coexist by the rules in
`bopa/docs/recognized-text.md`: text describing newer ink always wins, and identical text is
never rewritten.

That means you can run any combination. Devices off and scribe alone. Devices on for instant
results with scribe re-reading later. Or scribe only for pages the tablets did not read.

One thing worth knowing before you turn the tablets off: **the BOOX has an advantage scribe
cannot match.** MyScript reads pen trajectories — stroke order, direction, pen-lifts, timing —
while scribe renders those strokes to a bitmap and reads pixels. That information is discarded
before the model ever sees it, and no amount of compute recovers it. On the iPad side there is
no such gap: Vision is already reading a bitmap, so a better model here genuinely competes.

## Running it

```bash
cd deploy
cp .env.example .env && $EDITOR .env
docker compose up -d
docker compose run --rm scribe -check                # credentials and databases
```

The compose file runs a litellm sidecar as the `model` service, which presents the
OpenAI-compatible endpoint scribe speaks and forwards to whatever `deploy/litellm.yaml` maps.
Point `SCRIBE_MODEL_URL` elsewhere to use a different server; nothing in scribe changes either way.

`-check` verifies both databases are reachable and exits. `-once` takes a single batch and
exits, which is the way to watch a first run before letting it loose on the whole library —
though note that a batch is `SCRIBE_BATCH` changes, 200 by default, so on a first run that is
the whole library rather than a taste of it.

The first run reads every page in the library. That is the backfill. Against a hosted model it
is a couple of minutes; against a 3B model on a 6W CPU it is an overnight job.

### Settings

| Variable | Default | |
|---|---|---|
| `SCRIBE_COUCH_URL` | — | required |
| `SCRIBE_COUCH_USER` / `_PASSWORD` | — | the same `sync` account the tablets use |
| `SCRIBE_NOTES_DB` | `notes` | read only |
| `SCRIBE_TEXT_DB` | `notes_text` | read and written |
| `SCRIBE_MODEL_URL` | `http://127.0.0.1:11434/v1` | any OpenAI-compatible server |
| `SCRIBE_MODEL` | — | required; a model that server actually has |
| `SCRIBE_MODEL_STRONG` | — | optional; re-reads what `SCRIBE_MODEL` appears to have fumbled |
| `SCRIBE_MODEL_TIMEOUT` | `5m` | a timeout that fires mid-read costs the whole page |
| `SCRIBE_DEVICE_ID` | `server` | must differ from `boox` and `ipad` |
| `SCRIBE_POLL` | `30s` | wait when the feed is quiet |
| `SCRIBE_STATE` | `/var/lib/scribe/state.json` | the change-feed checkpoint |

## The model

Scribe does not host inference. It POSTs an image to an OpenAI-compatible
`/chat/completions` endpoint, which nearly everything speaks: llama.cpp's server, vLLM, LM
Studio, LocalAI, and Ollama at its `/v1` path. Swapping models is a config change.

On an N100, the thing that matters is that the iGPU is reachable through **OpenVINO**, not
through Ollama — Ollama will run on the CPU and be slow. Candidates worth trying, smallest
first:

- **Florence-2-large** (0.77B) — small, strong at OCR, runs well under OpenVINO.
- **TrOCR** handwritten — purpose-built for handwriting, but takes *line* crops rather than
  pages, so it needs a segmentation step scribe does not currently do.
- **Qwen2.5-VL-3B** quantized — whole-page input and good at layout; the default in the
  compose file.

Expect tens of seconds per page on this hardware. That is fine for a batch and useless for
anything interactive, which is why nothing here is on a request path.

### Two models, and when the second one runs

Set `SCRIBE_MODEL_STRONG` and every page is read by `SCRIBE_MODEL` first, with a second reading
only for pages whose first answer carries a sign of having gone wrong: non-Latin script, a
decoder stuck repeating itself, or the `[?]` markers the prompt asks for where a word is
illegible. Most pages never escalate, so the bill stays near the cheap model's.

What it is guarding against is not a model returning nothing. It is a small model handed
handwriting it cannot read returning fluent, confident, invented text — which in a vault is
worse than a gap, because afterwards nothing about it looks like an error. A 3B model asked to
read a line of Chinese here produced a grammatical sentence about a sister and her brother that
appears nowhere on the page.

Note the limit: the signals are all visible in the answer alone, so this catches a model that
*shows* it is struggling. It does not catch a confidently wrong reading of Latin script — a
misread date comes back looking exactly like a correct one. If that matters more than the cost,
make the better model `SCRIBE_MODEL` and leave `SCRIBE_MODEL_STRONG` empty.

The `engine` field on each document records which model actually produced it.

The prompt is configurable via `SCRIBE_PROMPT`. The default is blunt on purpose — left to
themselves these models editorialize, and "The image shows handwritten notes that appear to
say..." lands in someone's vault as though they had written it. Scribe also strips fences and
preambles from the answer, because asking politely only gets you most of the way.

## How it decides what to read

A page is read when its text is missing or **stale** — when the page's `updatedAt` is later
than the `recognizedClock` on its text. That clock is the page's own timestamp, copied verbatim
at the moment the ink was read, which is what makes the comparison meaningful across three
machines with three different clocks.

The page's clock is read *before* its ink, so ink that arrives mid-recognition leaves the page
looking stale afterwards and gets picked up next time. Reading it after would stamp that ink as
already read, and it would never be looked at again.

The checkpoint moves only after a batch is fully handled. An interruption re-reads rather than
skips; re-reading is cheap because the staleness check finds nothing to do.

## Development

```bash
go test ./...                                   # decoder, renderer, publishing rules
SCRIBE_TEST_COUCH_URL=http://127.0.0.1:5984 \
SCRIBE_TEST_COUCH_USER=<user> \
SCRIBE_TEST_COUCH_PASSWORD=<password> go test ./...  # plus the worker against a real CouchDB
```

The worker tests self-skip without a server. They create and drop their own scratch databases.

`internal/sb` is the third implementation of Notable's stroke format, after the BOOX's
`StrokePointConverter.kt` and the iPad's `SBStrokeCodec.swift`. Its golden payloads in
`testdata/` were produced by the iPad's encoder — a third reading of a binary format is exactly
where a plausible misinterpretation survives review, so it is checked against one that is
already in production rather than against my own reading of the spec.
