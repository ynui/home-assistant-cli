# home-assistant-cli (`has`)

[![CI](https://github.com/ynui/home-assistant-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/ynui/home-assistant-cli/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

A small command-line client for [Home Assistant](https://home-assistant.io),
installed as `has`. Control devices and check state; nothing more.

![the TUI](demo/demo.gif)

## Install

```sh
brew install ynui/tap/home-assistant-cli   # provides home-assistant-cli and has
```

The `has` name also exists in homebrew-core; `brew unlink has` first if needed.

```sh
go install github.com/ynui/home-assistant-cli@latest
ln -sf "$(go env GOPATH)/bin/home-assistant-cli" "$(go env GOPATH)/bin/has"   # optional
```

Or `make install` from a checkout (puts both names in `~/.local/bin`), or grab a
binary from the [releases](https://github.com/ynui/home-assistant-cli/releases).

Needs Go 1.26+ to build. Three dependencies: bubbletea, lipgloss, and a
WebSocket client (stdlib has none).

## Setup

Get a long-lived access token from your HA profile (`<url>/profile`), then:

```sh
has login
```

It prompts for URL and token, verifies them, and writes `~/.config/has/config.json`
(mode `0600`). Re-run to replace a revoked token.

## Commands

```
has groups
has areas [selector...]
has list [selector...] [--sort entity|name|state|changed|updated] [--reverse] [--limit N] [--brief]
has get <entity>
has on|off|toggle <selector...> [--all] [--dry-run]
has call <domain.service> [k=v...] [--all] [--dry-run]
has history <entity...> [--since 7d]
has logbook [selector...] [--day today|yesterday|YYYY-MM-DD] [--limit N]
has watch [selector...] [-o json]
has services [pattern]
has template <jinja>          # reads stdin if omitted
has ui [--expand] [--poll N] [--mouse] [--view groups|areas|states|flat]
has version
```

Global flags: `-o table|json`, `--url`, `--token`, `--timeout`.

`logbook` is "what just happened" (recorded events, including automations); `history`
is state changes for named entities. `watch` tails them live over the WebSocket.

## Selectors

An exact entity id, a domain (trailing `s` stripped), or a fuzzy substring of
the id or friendly name:

```sh
has list light.kitchen_light   # exact
has list lights                # domain
has list kitchen               # fuzzy
```

Reads accept many matches; writes need `--all` when more than one matches.
`--dry-run` resolves and prints without calling.

## Scripting

Results go to stdout, warnings to stderr; `-o json` on every command. Exit codes:
`0` ok, `1` error, `2` bad invocation, `3` selector matched nothing/too many,
`4` token refused, `5` server unreachable. Env: `HAS_URL`, `HAS_TOKEN`, `HAS_CONFIG`.

`list -o json` returns everything the server sent; `--brief` returns just
`entity_id`, `name`, `state`. `template` prints its result verbatim (so
`... | jq` works). `has groups` is a cheap starting point (~a few hundred bytes).

## The TUI

`has ui` shows actionable entities only (read-only hidden); search reaches all.
Every run starts with groups folded.

Header: `shown/total · folded · live|polling|connecting`. Live via WebSocket,
with polling fallback for the highlighted row; reconnects on its own.

| key | action |
|---|---|
| `/` | search (all printables become filter text) |
| `↑`/`↓`, `pgup`/`pgdn`, `home`/`end` | navigate |
| `←`/`→` | fold / unfold group |
| `enter` | toggle group, or open action menu |
| `space` | primary action |
| `1`-`9` | run that action (menu open) |
| `f` | favorite / pin |
| `v` | cycle grouping: groups, areas, states, flat |
| `ctrl+d` | entity attributes panel |
| `alt+↑`/`alt+↓` | move group or favorite |
| `ctrl+a` | toggle read-only entities |
| `ctrl+u` / `ctrl+r` | clear filter / refresh |
| `?` | help overlay |
| `esc` | close panel, filter, or fold group |
| `ctrl+c` | quit |

Favorites are a copy: pinned entities also stay in their own group, marked `★`.
Group order, favorites and view persist in `~/.config/has/ui.json` (no secrets).
Fold state does not persist. `--expand` opens all groups; `--mouse` enables clicks.

## Development

```sh
make            # fmt, vet, lint, test, build
make run ARGS="list kitchen"
```

`demo/fakeha` is a canned Home Assistant for trying changes without a real house:

```sh
go run ./demo/fakeha &
HAS_URL=http://127.0.0.1:8123 HAS_TOKEN=demo ./bin/home-assistant-cli ui
```

`make demo` re-records `demo/demo.gif` with vhs. Test fixtures are copied from
real HA responses.

## Notes

- Config: `~/.config/has/config.json`; presentation: `~/.config/has/ui.json`.
  `HAS_URL`, `HAS_TOKEN`, `HAS_CONFIG` override; flags override those.
- Long-lived token only; no OAuth.
- Interactive `login` uses `stty`, so that prompt is Unix-only.
- Areas come from one template render (HA keeps them out of `/api/states`).
  Most entities have none.

## License

MIT — see [LICENSE](LICENSE).
