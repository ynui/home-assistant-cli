# home-assistant-cli

[![CI](https://github.com/ynui/home-assistant-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/ynui/home-assistant-cli/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)
[![go](https://img.shields.io/badge/go-1.26-blue)](go.mod)

A small command-line client for [Home Assistant](https://home-assistant.io),
installed as `has`.

![the TUI: unfold a group, act on a light, pin it, cycle the grouping](demo/demo.gif)
*(recorded against a canned house — see [Development](#development))*

Built for one job: **control your devices and check what they are doing.**
It reads and writes — entities, services, the logbook, areas — and stops there:
no devices, no automations, no registries, no setup wizard. Just talk to the
house.

```
$ has list kitchen
ENTITY                  NAME               STATE
light (2)
light.kitchen_light     Kitchen Light      on
light.kitchen_light_2   Kitchen Light 2    off

$ has on lights --all
on will act on 5 entities
light.kitchen_light → on
light.kitchen_light_2 → on
light.hall_light → on
light.porch_light → on
light.garage_light → on

$ has get light.kitchen_light
ENTITY              NAME           STATE  CHANGED
light.kitchen_light  Kitchen Light  on     2026-10-16 14:02

$ has history light.kitchen_light --since 7d
ENTITY               STATE        CHANGED
light.kitchen_light  on           2026-10-09 18:09
                     unavailable  2026-10-09 22:34
                     off          2026-10-10 00:57

$ has logbook hallway --limit 3
WHEN                 ENTITY                                  NAME                     CHANGE
2026-10-17 09:37:31  binary_sensor.hallway_motion_occupancy  Hallway Motion Occupancy  on
2026-10-17 09:37:40                                          Hallway Motion Occupancy  off
2026-10-17 09:40:41                                          Hallway Motion Occupancy  on

$ has watch light.hall
  WHEN                 ENTITY                 CHANGE
2026-10-17 10:32:29  light.hall_light         off → on
2026-10-17 10:41:02  light.hall_light         on → off

$ has template '{{ states("sensor.hall_temperature") | round(1) }}'
21.4
```

## Install

```sh
brew install ynui/tap/home-assistant-cli     # installs home-assistant-cli and has
```

The `has` alias shares a name with an unrelated formula in homebrew-core. If you
already have that one, `brew unlink has` first.

Or with Go, which gives you the same two names:

```sh
go install github.com/ynui/home-assistant-cli@latest
ln -sf "$(go env GOPATH)/bin/home-assistant-cli" "$(go env GOPATH)/bin/has"   # optional alias
```

Or from a checkout: `make install` puts `home-assistant-cli` in `~/.local/bin`
next to a `has` symlink.

Binaries for macOS, Linux and Windows are attached to each
[release](https://github.com/ynui/home-assistant-cli/releases).

Needs Go 1.26+ to build. Three dependencies, all for the TUI: bubbletea and
lipgloss for drawing, and a WebSocket client — the standard library has none, and
the alternative is ~200 lines of RFC 6455 framing to own forever.

## Setup

Get a long-lived access token from your Home Assistant profile page
(`<your-url>/profile`, scroll to *Long-Lived Access Tokens*), then:

```sh
has login
```

It asks for the URL and token, hides what you type, verifies the credentials
before saving anything, and writes `~/.config/has/config.json` (mode `0600` —
it is a bearer token that grants full control of your house). Re-run
`has login` any time to replace a revoked token.

## Commands

| | |
|---|---|
| `has groups` | entity domains and counts, busiest first |
| `has areas [selector…]` | areas and the entities in them, busiest first |
| `has list [selector…]` | entity states as a table, `-o json` for full attributes |
| `has get <entity>` | exactly one entity |
| `has on \| off \| toggle <selector…>` | act on entities; each one is confirmed with its new state |
| `has call <domain.service> [k=v…]` | any service, for anything the shortcuts miss |
| `has history <entity…>` | recent state changes for the entities you name |
| `has logbook [selector…]` | what happened today, house-wide or for a selector |
| `has watch [selector…]` | tail state changes off the WebSocket, as they happen |
| `has services [pattern]` | callable services |
| `has template <jinja>` | render a template on the server, result printed verbatim |
| `has ui` | interactive browser |

Global flags: `-o table\|json`, `--url`, `--token`, `--timeout`.

`logbook` takes `--day today|yesterday|YYYY-MM-DD` and `--limit N`, and is the
answer to "what just happened": it is what Home Assistant recorded about
everything, including automations and scripts that have no state of their own.
`history` is the same idea narrowed to entities you name and a window you pick.

`watch` is that answer while it happens. It subscribes to state changes over the
WebSocket rather than polling, prints one line per change with the timestamp
Home Assistant recorded, and takes the same selectors as everything else. It
keeps its subscription if the socket drops — a house that restarts should not
need a restart — and gives up on a server it could never reach. `-o json` prints
one object per line for a pipe.

`template` is the escape hatch for questions with no endpoint: it posts to the
template renderer and prints the result exactly as it came back, so
`has template ... | jq` works and a template that renders json gives you json.
With no arguments it reads the template from stdin.

Writes report what they changed rather than just what they called: the states
come back with the service call, so nothing needs a following `has get`. With
`-o json` they are printed as JSON on stdout, which makes the write commands
scriptable.

`list` also takes `--sort entity|name|state|changed|updated`, `--reverse` and
`--limit N`. The timestamp column only appears when you sort by `changed` or
`updated`, so the column and the ordering always agree.

### Selectors

Every command takes the same three-step selector: an exact entity id, then a
domain, then a fuzzy substring on the id or friendly name.

```sh
has list light.kitchen_light   # exact
has list lights                # the light domain (trailing "s" is stripped)
has list kitchen               # fuzzy, matches friendly names too
has history kitchen --since 7d # history takes selectors too
has logbook kitchen            # so does logbook
has areas kitchen              # or the area name, since that is what it prints
```

**Reads accept many matches. Writes do not.** Anything matching more than one
entity needs `--all`, so a fuzzy name can never fire at the wrong thing:

```sh
$ has on kitchen
has: "kitchen" matches 2 entities:
  light.kitchen_light
  light.kitchen_light_2
name one exactly, or add --all
```

## The TUI

`has ui` shows what you can actually act on — sensors, zones, weather and other
read-only entities are hidden, which on a typical instance cuts a few hundred
rows to a few dozen. Search still reaches everything.

Every run starts with the groups closed, so what you act on is one keypress
away and the house does not open at 387 rows.

```
 ──────────────────────────────────────────────────────────────────────────
  http://homeassistant.local:8123  ·  127/387 · 13 folded  ·  live
 ──────────────────────────────────────────────────────────────────────────
  ▸ ▾ automation (26) 24 on
    ▸ media_player (24) 1 on
    ▸ switch (17) 3 on
    ▸ light (5) 1 on
```

The header counts what the list can reach against what the house holds. On a
typical instance the default view hides four fifths of the entities, and
`127/387` is the difference between a short list and a missing one.

**Live.** The list updates from Home Assistant's WebSocket, not from polling:
after an action the row changes the moment the server applies it, and a group
count is as fresh as the server rather than as fresh as the last time you looked
at that group. The header says `live`. If the socket cannot be opened — a proxy
that blocks the upgrade, for instance — it falls back to re-reading the
highlighted row every `--poll` and says `polling`, then reconnects on its own if
the connection drops later.

| key | |
|---|---|
| type | outside search mode: ignored — `/` starts search mode |
| type | inside search mode: filter (all letters, digits, space and symbols) |
| `↑` `↓` | move |
| `pgup` `pgdn`, `ctrl+f` `ctrl+b` | move a screenful |
| `home` `end` | first / last entity |
| `←` `→` | fold / unfold the highlighted group |
| `/` | start searching; while searching every printable is filter text, `esc` clears |
| `enter` | open the action sub-menu (sub-menu stays open after each run) |
| `space` | run the primary action (outside search) / run the selected action (sub-menu) |
| `1`-`9` | run that action while the sub-menu is open |
| `f` | toggle a favorite (outside search) |
| `v` / `ctrl+v` | cycle grouping view: groups (by domain), areas, states, flat |
| `ctrl+d` | show everything the server knows about the entity |
| `⌥↑` `⌥↓` (alt) | move the highlighted group one place, or a favorite within `favorites` |
| `esc` | close: the sub-menu, then the details, then the filter, then one more group |
| `ctrl+u` / `ctrl+r` | clear the filter / refresh |
| `ctrl+a` | toggle read-only entities in the list |
| `?` | show this table; closes itself |

Letters outside search mode are bound (`f`, `v`, `?`) because typing only
happens inside a search, where they are still filter text. There is no `q`,
`j`/`k` or `g`: those would be filter text you can never type. `⌥` is alt: on a
Mac keyboard it is the **Option** key next to the space bar. `shift+arrows`
scroll rather than reorder — reordering writes to `ui.json`, and that should not
be something you can do by accident while scrolling. `esc` never quits and never
opens anything: it closes the panel, then the filter, then folds the group under
the cursor and steps past it — hold it and the whole list shuts. `ctrl+c` is the
only exit.

`⌥↑`/`⌥↓` moves whatever the cursor is on. On a group header it swaps that group
with its neighbour in the order on screen; in the `favorites` group it moves
that favorite up or down. Each view keeps its own order — domains, areas and
states are saved separately, because the names live in different spaces — and
your favorites are listed in the order you pinned them, not alphabetically. An
entity that is neither a group nor a favorite has no order of its own, so there
the keys say so instead of guessing. `flat` has a single group, so there is
nothing to move there.

`enter` over a group header folds / unfolds it as well.

`ctrl+d` lists everything the server knows about the entity — brightness, colour
modes, whatever the integration recorded — and trims itself to half the screen so
the list stays visible behind it. Battery, when there is one, goes first as
`battery 42%` rather than sitting in the alphabetical dump where it is never
found. `?` is a popup over the list rather than a panel under the cursor, so it
costs the list nothing and moves nothing when it closes.

While the action sub-menu is open the menu owns the keyboard: `1`-`9` run that
entry directly, the menu stays open after each run, and no printable key can be
typed into the filter, so an action can never fire on a different entity than the
one you opened the menu on. A favorite is toggled with `f` (outside search) or
with the `favorite`/`unfavorite` entry in its action menu.

A favorite is a second copy, not a move: it joins the starred `★ favorites`
group at the top and stays in its own domain, area or state, marked `★` on the
row there. Pinning something does not make it disappear from where it was — the
groups view used to do exactly that, which read as deleting the entity.

Action lists come from your own server's service registry, so they show what
that domain really offers. A lock gets `lock`, `unlock`, `open`; a cover gets
`open_cover`, `close_cover`.

The order of your groups in each view, the order and membership of your favorites,
and which grouping view you left it in all persist in `~/.config/has/ui.json` —
that file holds nothing else, and no secrets. A swap writes the whole order, so a
filtered or read-only-hidden group is never forgotten by a move you cannot see.
Fold state does not persist: every run starts with everything closed, so the list
is the same size every morning whether or not you left it open yesterday.
`--expand` is there for the days you want it open.

A favorite is offered in the action sub-menu of anything that has other actions,
which is most things; `f` pins any entity, including one with nothing to act on.

Flags: `--expand` (start with every group open), `--poll 0` (no live updates at
all), `--mouse` (click to select, wheel to scroll — enabling it takes the
pointer away from terminal text selection), `--view` (grouping: `groups`,
`areas`, `states`, or `flat`).

## Notes

- Config: `~/.config/has/config.json`, presentation: `~/.config/has/ui.json`.
  `HAS_URL`, `HAS_TOKEN` and `HAS_CONFIG` override them; flags override those.
- Password-free: a long-lived token only. No OAuth, no credential storage.
- Interactive `login` uses `stty` to hide the token, so that prompt is
  Unix-only.
- Areas come from one template render of `/api/template`, because Home Assistant
  keeps them out of `/api/states`. Most entities have none and are not listed by
  `has areas`, or grouped anywhere but `unknown` in the TUI. The device registry
  is still not used.
- `has watch` is the only command with no polling fallback: a tail with no socket
  has no list to keep fresh, so a server it cannot subscribe to is an error
  rather than a slower mode. `has ui` is the other way round — it degrades to
  polling because it has something to show either way.

## Development

```sh
make            # gofmt, go vet, go test -race, build
make run ARGS="list kitchen"
```

One package, one binary, and no linter beyond `go vet`. `demo/fakeha` is a canned
Home Assistant — a flat `/api/states`, a `/api/services` grouped the way the real
one groups it, the WebSocket handshake, and entities that change on their own:

```sh
go run ./demo/fakeha &                 # http://127.0.0.1:8123, token: demo
HAS_URL=http://127.0.0.1:8123 HAS_TOKEN=demo ./bin/home-assistant-cli ui
```

It is what `make demo` records against — `vhs demo/demo.tape`, needs `vhs` with
`ttyd` and `ffmpeg` under it — and it is the quickest way to try a change without
touching a real house. Test fixtures in `*_test.go` are copied from real Home
Assistant responses for the same reason: a mock that disagrees with the server is
worse than no mock.

## License

MIT — see [LICENSE](LICENSE).