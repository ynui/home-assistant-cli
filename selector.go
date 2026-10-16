package main

import (
	"fmt"
	"sort"
	"strings"
)

// A token from the command line is resolved in three steps: an exact entity
// id, then a group (the entity's domain), then a fuzzy substring match on the
// entity id or friendly name.

// domain is the part of an entity id before the dot.
func domain(entityID string) string {
	if i := strings.IndexByte(entityID, '.'); i >= 0 {
		return entityID[:i]
	}
	return entityID
}

// resolve turns tokens into entities.
//
// Reads pass all=true and get whatever matched. Writes pass all=false, which
// refuses any token matching more than one entity unless --all was given: a
// token matching several entities is by definition not an exact id, so acting
// on them would be a guess.
func resolve(states []State, tokens []string, all bool) ([]State, error) {
	var out []State
	seen := map[string]bool{}
	for _, tok := range tokens {
		matches := match(states, tok)
		switch {
		case len(matches) == 0:
			return nil, fmt.Errorf("no entity matches %q; run `has list` to see everything, or `has groups` for the domains", tok)
		case len(matches) > 1 && !all:
			return nil, tooMany(tok, matches, isGroup(states, tok))
		}
		for _, s := range matches {
			if !seen[s.EntityID] {
				seen[s.EntityID] = true
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// tooMany explains why a token was refused and lists enough candidates to
// choose from without flooding the terminal.
func tooMany(tok string, matches []State, group bool) error {
	if group {
		return fmt.Errorf("%q is a group of %d entities; add --all to act on all of them", tok, len(matches))
	}
	return fmt.Errorf("%q matches %d entities:\n%s\nname one exactly, or add --all",
		tok, len(matches), candidates(matches))
}

// candidates lists entity ids, capped so a broad name cannot flood the output.
func candidates(matches []State) string {
	const shown = 8
	var lines []string
	for i, s := range matches {
		if i == shown {
			lines = append(lines, fmt.Sprintf("  ... and %d more", len(matches)-shown))
			break
		}
		lines = append(lines, "  "+s.EntityID)
	}
	return strings.Join(lines, "\n")
}

func match(states []State, tok string) []State {
	for _, s := range states {
		if s.EntityID == tok {
			return []State{s}
		}
	}
	if g := matchGroup(states, tok); len(g) > 0 {
		return g
	}
	return matchFuzzy(states, tok)
}

// matchGroup matches a domain name, tolerating one trailing plural "s" so that
// "lights" finds the "light" domain.
func matchGroup(states []State, tok string) []State {
	name := strings.TrimSuffix(tok, "s")
	var out []State
	for _, s := range states {
		if domain(s.EntityID) == name {
			out = append(out, s)
		}
	}
	return out
}

func isGroup(states []State, tok string) bool {
	for _, s := range states {
		if s.EntityID == tok {
			return false
		}
	}
	return len(matchGroup(states, tok)) > 0
}

func matchFuzzy(states []State, tok string) []State {
	needle := strings.ToLower(tok)
	var out []State
	for _, s := range states {
		if strings.Contains(strings.ToLower(s.EntityID), needle) ||
			strings.Contains(strings.ToLower(friendlyName(s)), needle) {
			out = append(out, s)
		}
	}
	return out
}

func entityIDs(states []State) []string {
	out := make([]string, 0, len(states))
	for _, s := range states {
		out = append(out, s.EntityID)
	}
	return out
}

// targetKeys are the payload keys that name entities. `has call` resolves them
// through the same rules as every other selector, so the one command that can
// fire anything cannot be the one that skips the check.
var targetKeys = []string{"entity_id", "entity_ids"}

// resolveTargets expands entity_id / entity_ids values through match, so
// `has call light.turn_on entity_id=kitchen` means what every other selector
// means. Two deliberate differences from a plain resolve:
//
//   - a token matching more than one entity is refused without --all, the same
//     rule writes get, because a service call is a write;
//   - a token matching nothing is passed through untouched. entity_id is not
//     always an entity id (notify.* takes a device id), and Home Assistant is
//     the authority on what that key means.
func resolveTargets(c *Client, data map[string]any, all bool) error {
	var keys []string
	for _, k := range targetKeys {
		if s, _ := data[k].(string); s != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	states, err := c.states()
	if err != nil {
		return err
	}
	for _, k := range keys {
		var out []string
		for _, tok := range strings.Split(data[k].(string), ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			matched := match(states, tok)
			switch {
			case len(matched) == 0:
				out = append(out, tok)
			case len(matched) > 1 && !all:
				return tooMany(tok, matched, isGroup(states, tok))
			default:
				out = append(out, entityIDs(matched)...)
			}
		}
		data[k] = strings.Join(out, ",")
	}
	return nil
}

// groupCount is one row of `has groups`.
type groupCount struct {
	Domain string `json:"domain"`
	Count  int    `json:"count"`
}

// groupCounts counts entities per domain, busiest first.
func groupCounts(states []State) []groupCount {
	counts := map[string]int{}
	for _, s := range states {
		counts[domain(s.EntityID)]++
	}
	out := make([]groupCount, 0, len(counts))
	for d, n := range counts {
		out = append(out, groupCount{d, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Domain < out[j].Domain
	})
	return out
}

// domainCounts is groupCounts reduced to a lookup table.
func domainCounts(states []State) map[string]int {
	out := map[string]int{}
	for _, s := range states {
		out[domain(s.EntityID)]++
	}
	return out
}
