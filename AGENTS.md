# AGENTS.md

Go CLI (`has`) for Home Assistant. Single `package main`, no framework except
bubbletea/lipgloss for the TUI, plus one WebSocket client — the stdlib has no
WS client, and hand-rolling RFC 6455 is ~200 lines of framing to own forever
(`ws_test.go` also has a fake server so the handshake is testable).
Everything else is stdlib — keep it that way unless there is a real reason.

## Commands

```sh
make            # fmt -> vet -> lint -> test -> build, in that order
make lint       # staticcheck, as a module download: nothing to install
make vuln       # govulncheck; needs the network, so it is not in `all`
make build      # -> bin/home-assistant-cli   (never leave a binary in the repo root)
make install    # -> ~/.local/bin, then verifies it is on PATH
make uninstall  # removes exactly that, idempotently
make run ARGS="list kitchen"
make demo       # re-record demo/demo.gif with vhs, against demo/fakeha
```

`make install` is the only target that can shadow a brew install: it puts the
binary and a `has` symlink in `~/.local/bin`, which usually precedes
`/opt/homebrew/bin`. It therefore warns when another `has` already exists on
PATH, and `make uninstall` gives the name back. Developing needs neither — `make
run` / `go run .` use the checkout and leave PATH alone — so a brew-installed
`has` and a dev checkout can coexist, which is the recommended arrangement.
`brew upgrade` needs `brew update` first: the tap clone is cached, so it will
otherwise report nothing to upgrade and quietly leave you on the old version.

Focused checks: `go test -run TestUISubMenu ./...`, `go test -run TestSelfCheck -v ./...`
(one top-level test with `t.Run` subtests). No suite runner, no fixtures on
disk. `TestSelfCheck` in `main_test.go` covers the non-TUI logic. The read
commands' tables are pinned by `TestReadCommandOutput` in `output_test.go`, and
`BenchmarkUIRows`/`BenchmarkUIView` answer "is the per-keystroke list rebuild
worth caching?" with numbers rather than argument.

**`TestMain` points `HAS_CONFIG` at a temp directory for the whole package, and
that is load-bearing.** The UI writes `ui.json` when you pin, reorder or switch
view, and all three are reachable from a plain `m.Update(...)`, so a test that
forgets to redirect the config writes to the real `~/.config/has/ui.json` — and
a zero-valued `uiConfig` replaces the user's favorites and group order with
`{"view": "..."}`. That happened, on every run, to the house this was written
against. A test that wants its own path sets `HAS_CONFIG` with `t.Setenv`; do not
assume a test is harmless because it "only reads the model".

## Hard-won constraints

**Search is modal, and that is what makes letters safe to bind.** Typing
`kitchen` once lost its `k` to a vim-style up-binding. `_` and `-` were bound to
on/off, which made entity ids like `light.hallway_light` untypable. Now `/` is
the only way into search, typing outside it does nothing, and inside it every
printable is filter text — so `f`, `v` and `?` are bound outside search without
costing a single character of the filter. Anything reachable by letter must
still check `!m.searching` first, or it fires while someone is typing. There is
no `q`, `j`/`k` or `g` for the same reason: they would be filter text you could
never type. Reordering lives on `alt+arrows` rather than `shift+arrows`
(reordering writes to `ui.json` and must not be reachable by a reflex meant for
scrolling), and the details panel is `ctrl+d` because a panel spliced under the
cursor moves the list, while `?` is fine as an overlay that replaces lines.
`alt+↑/↓` are indistinguishable from plain arrows in some terminals, where the
key then does nothing — that is the failure mode, and it is the safe one.

**esc only ever closes.** It closes the sub-menu, then the details, then the
filter, then folds the group under the cursor and steps past it — never quits,
never unfolds. A press that could also unfold makes two presses undo each other,
and "press esc until it is all shut" is the behaviour worth having; that is also
why there is no fold-all key at all, `tab` used to be one. `ctrl+c` is the only
exit.

**The websocket is the live path; polling is the fallback.** `ws.go` speaks the
five-step exchange (`auth_required` → `auth` → `auth_ok` → `subscribe_events` →
`result`), verified against a real HA 2026.9.4: the subscription is only treated
as live once the `result` ack arrives, and `auth_invalid` is final rather than
retried. A refused handshake retries `wsConnectTries` times and then settles for
polling; a *dropped* socket reconnects forever with a capped backoff, because a
house that comes back should not need a restart. `liveState()` in the header
must never imply freshness it does not have. `TestWS*` covers all of it.

**Two readers, one handshake, and `wsSend` stays tiny.** The TUI wants a
`state_changed` decoded into typed fields on the hot path; `has watch` wants
every event's raw bytes and no model at all. So the *send* side is shared and
still four fields (`wsSend` — nothing may be added to it), the subscription takes
its event type as an argument (empty = every event, which Home Assistant accepts
with the field omitted, verified live), and the read side is two structs on
purpose: `wsRecv` decodes state_changed for the model, `wsEvent` keeps the raw
payload for anyone who is not the model. Merging them costs the TUI an unmarshal
per event to get bytes it throws away. `wsStream` is not a `tea.Cmd` — nothing
else drives it, and its `connected` return is what lets `watch` tell a dead host
(it gives up after `wsConnectTries`) from a dropped socket (it reconnects
forever); `wsBackoff` is shared with `uiModel.backoff` for the same reason.

**`has template` prints its result verbatim, in any output format.** It posts to
the same `/api/template` render `entityAreas` uses — one endpoint, one body
shape, one place to fix. `-o` does not apply: a template can render a number, a
yaml document or a word, and wrapping it would break the `| jq` that is the whole
reason the command exists. When the template *is* json, the output is json.

**Send and read are separate types, and that is not tidiness.** Home Assistant
validates every message against a schema and *rejects unknown keys*, so the one
struct that did both directions sent `success` and `event` inside the auth
message and every handshake died with "Auth message incorrectly formatted" —
while the fake server in `ws_test.go` accepted it happily. `wsSend` stays small,
and `wsUnknownKey` makes the fake refuse what the real server refuses. The
handshake gets `--timeout`, not the poll interval; three seconds is fine to
re-read one row and not fine to authenticate.

**Every read resolves its tokens through `resolve()`.** `history` used to pass
the token straight to `filter_entity_id`, so `has history kitchen` asked HA for
an entity called "kitchen", got `[]` and exited 0 — indistinguishable from an
entity that never changed. Pinned by `TestHistoryResolvesSelectors`, which
drives `cmdHistory` against a server and reads the query it sent.

**A service call answers with the states it changed, and that body is the only
confirmation a write ever gets.** `Client.call` returns them, `cmdCall` prints
`entity → state`, and the TUI reads the new state from the same response instead
of re-fetching. Discarding it puts every write back to "hope it worked".

**Column widths must use `pad()`, never `fmt`'s `%-13s`.** `%` pads by *bytes*
and `…` is three bytes, so every truncated value silently shifted its column.

**`exec.Command` needs `cmd.Stdin = os.Stdin` when shelling out.** `os/exec`
wires a nil `Stdin` to `/dev/null`, which made `stty -echo` fail and would have
echoed the user's token in cleartext. Only one call site, `stty()`.

**Test fixtures must be copied from a real Home Assistant response.** This has
now bitten twice. `has services` shipped returning empty rows because the mock
used a flat `{domain, service}` shape while `/api/services` returns services
*grouped* under `services: {turn_on: {...}}`. `has history` shipped 400-ing
because the mock accepted any path matching `/api/history/period/\d+`, while
real HA parses that segment as a **datetime string** and rejects a Unix epoch
with "Invalid datetime" — it wants RFC 3339. Both are pinned by tests; do not
simplify them back. When a mock disagrees with reality, reality wins: check the
live endpoint with curl before trusting a green test.

**Test the TUI through the model, not a pty.** bubbletea's alt-screen repaints
mean pty capture returns mangled or stale frames; it cost several wasted
debugging rounds. Every UI test drives `Update()`/`View()` directly and `t.Log`
the rendered screen.

**A bool flag never takes the next argument, and a positional keeps the order it
was written in.** `parseArgs` pre-splits because the stdlib parser stops at the
first positional, which would silently ignore `has history light.x --since 5m`.
It used to pull the following token in as a flag's value whatever the flag was,
so `has call light.turn_on --all entity_id=kitchen` read `entity_id=kitchen` as
the service to call, and every flag written after it was dropped — the parse had
already stopped. `endOfFlags`/`takesValue` ask the FlagSet whether a flag wants
a value (stdlib bools answer `IsBoolFlag` themselves) and `parseArgs` re-runs
`fs.Parse` on the remainder rather than splitting once. `TestParseArgsBoolFlags`
and `TestParseArgsFlagsStillApply` pin both halves.

**One context for the whole program, cancelled by ^C.** `Client.do`, the
websocket handshake, the socket reads and `has watch` all hang off a package-level
`ctx` that `signal.NotifyContext` cancels. It used to be `http.NewRequest` with
no context and `context.Background()` in `watch`, so ^C left a request waiting out
the thirty-second `--timeout` and killed a tail mid-line. Set it up in `main`
(`onInterrupt`), never in a command.

**The frame is derived, never stored.** `m.frame(rows)` computes the window, the
screen line the list starts on and the in-view group indicator; `View` renders
it and `rowAtY` reads it. `View` used to write `rowTop`/`winStart` back into the
model as a side effect, and `chromeLines()` re-derived what it had written, with
a comment admitting the two had to match. One copy of the arithmetic, and a click
no longer depends on a `View` having happened first.

## Invariants worth preserving

- **Writes go through `resolve()`** (`selector.go`), which refuses any token
  matching >1 entity unless `--all`. Reads pass `all=true`. A new write command
  that bypasses this will fire at whatever a substring happened to match.
- **"Actionable" is derived from the server**, not a hardcoded domain table:
  `actions.go` keeps a service only if it targets an entity, the name is in
  `controlVerbs`, and it needs no required arguments. HA spells these
  inconsistently (`cover.open_cover` vs `lock.open`), so the verb list is
  explicit. Adding a domain's action means adding its verb there.
- **`configDir()` is `~/.config/has`, deliberately not `os.UserConfigDir()`**,
  which would land in `~/Library/Application Support` on macOS.
- **File modes**: `config.json` is `0600` in a `0700` dir and holds a bearer
  token; `saveConfig` calls `Chmod` explicitly because `WriteFile` only applies
  the mode on creation. `ui.json` is `0644` (no secrets).
- **The TUI refreshes lazily**: full `/api/states` on start, on `^R`, and on
  every reconnect (events missed while away are never delivered). While polling
  is the fallback it also does `GET /api/states/{id}` for the highlighted row
  only, debounced via a generation counter so stale replies are dropped.
- **Every run starts with every group folded.** Fold state is deliberately not
  persisted: a saved snapshot once made the list come back at 387 rows because
  yesterday ended with everything open. `ui.json` holds the three orders
  (`group_order`, `area_order`, `state_order`), `favorites`, `hidden` and
  `view` — preferences, never a snapshot; `--expand` is the opt-out for a run
  that wants it open. Per-entity hiding is the `h`/`H` pair: `h` toggles the
  highlighted row off the list (and on), `H` lists only what `h` hid, and the
  hidden view empties itself out — it leaves on the last unhide. It mirrors
  favorites exactly: `toggled(list, id)` in `uiconfig.go` is the one add-or-
  remove both share, `toggleFavorite`/`toggleHidden` copy the config and save
  it at once, and `follow(r)` puts the cursor back by identity. Hiding never
  makes a row unreachable: search reaches hidden entities (marked), the
  precedent `TestUIFilterFindsHidden` already pinned for the ^A filter. There
  is no hide-in-the-menu on purpose: a read-only sensor has no sub-menu, and
  `space` on a no-action entity must never become a hide.
- **`uiConfig` methods copy rather than construct.** A `moveGroup` that rebuilt
  the struct field by field used to drop any field added after it (`folded`, then
  `HiddenGroups`) from the file on every reorder. It is gone now — the swap needs
  the order the list is showing, which only the model knows — and `nudgeGroup`
  copies the config instead.
- **An overlay pads the view to `m.height` before it draws.** `?` is centred in
  the terminal, not in whatever the list happened to draw: with every group
  closed the view is seven lines, and a box centred in seven loses its own
  bottom half. Padding draws nothing new (bubbletea blanks those lines anyway)
  and cannot scroll the header away, because the view never grows past
  `m.height`.
- **One field per concept, and no alias fields.** `uiModel` once carried ten
  names for the grouping mode and four for the area map, with `currentView()`
  reading all ten — once per entity per `rows()` call. Nothing outside `ui.go`
  ever set them and no linter flags an unused struct field, so they were pure
  weight. One `view` string and one `areas` map, both set through
  `parseView`/`areasMsg`.
- **`group_order` is one concept: the order of the domains, top to bottom.** It
  used to be a partial *pin list* that `rank()` then used as a total order, and
  the two did not fit: with nothing pinned — every fresh install, and anyone who
  never pressed `alt+↑` — there was nothing to swap inside, so `alt+↓` could
  never do anything and the group at the top of the screen answered "already
  bottom". `nudgeGroup` now swaps the highlighted group with its neighbour *in
  the order on screen* and saves the whole order. Two rules that keep it honest:
  the swap is between visible groups only, because trading places with a domain
  `^A` is hiding reports a move that never happened on screen; and everything not
  on screen is appended, because a partial order forgets the hidden groups.
- **One order per view, in `orderFor`/`withOrder`.** `group_order`, `area_order`
  and `state_order` are three fields because the names live in different spaces —
  an area order saved as a domain order buries the domain one, which is what
  happened when the areas view refused to reorder at all. Both accessors copy
  rather than construct, so writing one view's order cannot drop the others, and
  `flat` (one group) falls through to the domain order harmlessly. The header
  carries no position number: when only a few groups were pinned `·3` was worth
  drawing, and once every domain was in the file it became `·6` on every row.
- **`favorites` is an ordered list, not a set.** `rows()` walks `cfg.Favorites`
  and keeps whatever is still in the current list, so the group is in the order
  the user pinned it; `⌥↑/↓` on a favorite swaps its neighbours in that slice.
  `nudge` is the one entry point: a header moves a group, an entity moves a
  favorite, and anything else says it has no order of its own rather than moving
  the domain behind your back. `alt` is `⌥` (Option) on a Mac, and every doc that
  mentions it says so.
- **The view must fit `m.height` or it loses its own header.** bubbletea keeps
  the *last* `height` lines of an over-long view (`standard_renderer.go`), so an
  under-sized reserve scrolls the title and filter line off the top and shifts
  the list under the cursor — it does not clip the help line. `chromeLines`
  counts the fixed frame; the rest of the body is split between the list and any
  panel by `panelHeight()` — half each — because the panel draws *inside* the
  list area, so sizing them independently overflows the terminal.
  `TestUIViewFitsTerminal` and `TestUIDetailsTrimsToFit` check it.
- **An entity with no `friendly_name` has no name.** The TUI leaves the name cell
  empty and puts the id in the id column, exactly as `has list` does. Falling
  back to the id inside the name column put every id at one of two indents and
  made `light.workspace_light` read like a name.
  `TestUINamelessEntityRendersIDNotName` pins the column both ways.

- **Search is modal (`/`).** Typing does nothing outside of search mode; `/`
  opens it, even a bare `/` press clears it, and `esc` and `enter` leave it.
  This supersedes the old "every letter is filter text" rule: letters are
  filter text now, but only inside a search. Space runs the primary action
  everywhere else and is free to be typed inside a search at the same time.
- **The sub-menu owns the keyboard while it is open.** `handleKey` returns early
  for anything `menuKey` does not consume, so a printable never reaches the
  filter and a keystroke cannot move the cursor out from under an action being
  fired. Same write-to-the-wrong-entity class of bug, fixed once in one place.
  Digits 1-9 run a menu entry directly; they are filter text everywhere else.
- **The frame fits both dimensions.** The 72-column rule, the status line, the
  help line and the in-view group indicator are all sized from `m.width`, after
  an over-wide rule wrapped on a 60-column terminal and scrolled the title off
  the top. `m.termWidth()` returns `defaultWidth` before the first
  `WindowSizeMsg`.
- **Only one panel draws at a time.** `panelLines()` returns the sub-menu when
  it is open and the details otherwise, and `listHeight()` subtracts just that:
  two panels each sized against half the body would leave the list nothing. No
  state is dropped either way.
- **`?` help is an overlay, not a panel.** `overlay()` centers it over the
  rendered view and *replaces* lines — it cannot add them, or the frame goes one
  line too long and the title scrolls off the top. It took no list space either:
  spliced in under the cursor row it read as a help screen the highlighted group
  had swallowed. `padTo`, not `pad`, because the rows are styled and `pad`
  counts ANSI escapes as visible columns. `?` closes the details panel, which
  the box lands on top of.
- **A websocket message carries the connection it came from.** `wsReadCmd` tags
  every `wsStateMsg`/`wsIdleMsg`/`wsGoneMsg` and the model drops a message whose
  `conn` is not `m.conn`. A read armed on an old socket used to report that
  socket's death as the live one's, closing it and reconnecting forever.
- **Writes go to stdout; warnings to stderr.** `has on`/`off`/`toggle`/`call`
  print `entity → state` or `called domain.service` on stdout, so the result can
  be captured and piped. `--all`'s blast-radius notice and a service that
  changes nothing still speak on stderr.
- **An exit code is the message a script gets instead of the message.** 1 error,
  2 bad command line, 3 selector resolved to nothing or too much, 4 token
  refused, 5 server unreachable. The code is carried by `coded`, which wraps the
  error instead of tagging its text — `fmt.Errorf("%w…", errUsage)` prints the
  sentinel's own message into the middle of the sentence, which is how the first
  attempt came out as `has: usagebad --sort "bogus"`. A new failure that a script
  could act on differently needs its own code, wrapped at the source; one that
  cannot stays 1. Pinned by `TestExitCodes`.
- **`list --brief` exists because attributes are half the payload.** `list -o
  json` stays the whole server answer — that is a fair thing to want — but on a
  387-entity house asking "which lights are on" costs ~30k tokens of
  `supported_features` nobody read. `--brief` emits the table's three columns as
  json and is 80% smaller. Pinned by `TestListBrief`.
- **`--dry-run` resolves and then stops, and that order is the feature.** It sits
  between the selector resolution and `c.call` in `cmdCall`, so it costs no extra
  request and still reports the ids the selector really matched — `has list
  kitchen` answers a different question. One insertion point covers on/off/toggle
  and `call`. Pinned by `TestCallDryRun`, which counts POSTs.
- **`has call` resolves `entity_id` / `entity_ids`.** Values go through
  `resolveTargets`, so the selector rules hold for service calls too; a value
  matching nothing is passed through untouched, because that key is not always
  an entity id.
- **`has services` shows the targets it already parsed.** `Service.entityDomains()`
  used to be thrown away; the new TARGETS column is what says whether a service
  can be aimed at an entity at all.
- **`has services` lists a single match's fields, and only a single match.** The
  registry already carries description, example and required, so there is no
  second request to make and no schema file to keep in sync — and no types or
  ranges to print, because HA does not send any. Many matches would leave the
  question "which one", and a field list after every row buries the table.
  Pinned by `TestReadCommandOutput`, which also asserts the many-match table has
  no FIELD header.
- **A state the UI patches in must be the whole state.** A service call answers
  with every entity it changed, and a light group is one of them, so
  `actionCmd` picks the entry matching the entity that was acted on — and passes
  the whole `State`, because `patch` replaces the struct and a bare
  `{entity_id, state}` takes `friendly_name` with it. That blanked the name on
  the row you had just pressed space on, and only the websocket event repaired
  it, so on the polling path it stayed blank until `^R`. `patch` now also keeps
  the attributes it already holds when a patch arrives without any.
- **Running entry *n* must use the list the menu drew.** `actionsFor` appends a
  `favorite` entry; `runAction` read the registry instead, so the last menu entry
  — the documented favorite — died on "light has 3 actions, 4 is not one", and
  the `__favorite__` branch in it was unreachable. That entry is only appended
  when the entity has a real action, because `space` runs entry 0 and must not
  quietly pin an entity that has nothing to act on.
- **A favorite is a copy, not a move.** `rows()` gathers favorites into the top
  group *and* leaves them in the group they came from, in every view — the
  groups view once dropped them outright, which made pinning an entity look
  like removing it from the house, and the areas view kept them, so the two views
  disagreed about the same list. `groupOrder()` counts them in their own group
  too, or a domain header claims fewer entities than it has. `favoritesGroup` is
  the only header that is not a domain, an area or a state, which is why it is
  the one marked `★`.
- **A cursor is held by identity, never by index.** Anything that reshapes the
  list — pinning, unpinning, reordering a group, switching grouping, areas
  arriving — shifts every index below it, so `m.cursor = i` after the change
  lands on whatever slid into that slot. `m.follow(was row)` re-finds the row and
  puts the cursor back on it. Identity is *entity plus group*, not entity alone:
  a favorite is in two groups at once, so matching the entity alone puts the
  cursor on the copy at the top of the list instead of leaving you where you
  were. If the exact row is gone — the copy unpinning just deleted, or a
  grouping that calls the group something else — the entity on its own is the
  next best answer, and only a row that no longer exists at all falls back to
  clamping.

## Environment

- `HAS_CONFIG` overrides the config path (used by tests via `t.Setenv`);
  `HAS_URL`/`HAS_TOKEN` override values. Precedence: flags > env > file.
- **The binary is `home-assistant-cli`; `has` is the alias next to it** (a
  symlink from `make install`, from the brew formula, and the name the program
  uses for itself in `usageText`, its error prefix and `~/.config/has`). The
  module is `github.com/ynui/home-assistant-cli`.
- **`demo/fakeha` is a canned Home Assistant**, not part of the client: it is
  the second `package main` and exists so the README gif can be re-recorded
  without a house. It answers the real shapes (flat `/api/states`, grouped
  `/api/services`, a list-of-lists history) and speaks the WebSocket handshake —
  `/api/websocket` is deliberately *not* behind the bearer-token check, because
  Home Assistant authenticates a socket with the token in the auth message.
  `demo/demo.tape` points `HAS_CONFIG` at `$(mktemp -d)/config.json`; a bare
  directory makes `configDir()` write `ui.json` one level up, shared with every
  other run.
- Git repo on `main`. `go vet` is the only linter `make` runs; CI
  (`.github/workflows/ci.yml`) is that plus `gofmt -l`, staticcheck,
  govulncheck and `go test -race -cover` on ubuntu and macos, and
  `.github/workflows/release.yml` builds the release archives on a `v*` tag and
  bumps `ynui/homebrew-tap/Formula/home-assistant-cli.rb`. Workflow actions are
  pinned to commits, and their majors track the runner's Node (node20 is
  force-run on node24 and going away). `.gitignore` covers
  `bin/` and a stray root binary; `make build` is the only sanctioned way to
  produce one.
- `has history` takes Go duration units plus `d`/`w` (`7d`, `2w`); plain
  `time.ParseDuration` rejects days, which the `--since` help text promises.
- Verified against a real HA 2026.9.4 (387 entities). The device registry needs
  the WebSocket API and is deliberately unused — the entity list and the service
  registry cover everything here. Areas are the exception: they come from one
  template render (`Client.entityAreas`), because `/api/states` does not carry
  them, and 63 of 387 entities have one.
- **`/api/logbook/<date>` is a GET and answers POST with 405.** The only filter
  is `?entity=`, comma-separated, and the entries are oldest-first. The shape
  varies per entry: a state change carries `state`, a script or automation run
  carries `message`, and an entry with no entity has neither, so only `when` and
  `name` are always present. It keeps seconds (`logbookFormat`) because one
  presence sensor managed 847 changes in a day — a minute-resolution column is
  a column of identical times.
- Hidden input for the token prompt uses `stty`, so interactive `login` is
  Unix-only. It compiles everywhere but fails at that prompt on Windows.
