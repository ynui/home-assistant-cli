package main

import "sort"

// The action registry turns Home Assistant's service list into "what can I do
// to this, and is it worth showing at all". Everything is derived from what
// the server advertises rather than a hardcoded domain table, so custom
// integrations work too.

// controlVerbs are the no-argument services that count as acting on an entity,
// in the order they should be offered. The first one a domain has becomes the
// action bound to space.
//
// Home Assistant does not name these consistently - covers use open_cover
// where locks use open, media players use media_play - so the list is spelt
// out rather than guessed.
var controlVerbs = []string{
	// the generic power verbs, preferred because they read the same everywhere
	"toggle", "turn_on", "turn_off", "press", "run", "start", "stop", "pause",

	// position and safety. lock/unlock lead because for a lock they are the
	// meaningful pair; open on a lock means something different again.
	"lock", "unlock", "open", "close",
	"open_cover", "close_cover", "stop_cover",
	"set_cover_position", "set_cover_tilt_position", "toggle_cover_tilt",
	"start_mowing", "pause_mowing", "dock",

	// playback and sound
	"play_pause", "media_play", "media_pause", "media_stop",
	"media_next_track", "media_previous_track",
	"volume_up", "volume_down", "mute", "unmute",

	// levels
	"set_percentage", "set_humidity", "set_temperature",

	// whole-machine movement
	"return_to_base", "clean_spot", "locate", "take_snapshot",
}

// verbOrder turns a verb list into a sort key; unlisted verbs sort last.
var verbOrder = func() map[string]int {
	m := make(map[string]int, len(controlVerbs))
	for i, v := range controlVerbs {
		m[v] = i
	}
	return m
}()

var verbLabels = map[string]string{
	"toggle": "toggle", "turn_on": "on", "turn_off": "off",
	"press": "press", "run": "run", "start": "start", "stop": "stop", "pause": "pause",
	"open": "open", "close": "close", "lock": "lock", "unlock": "unlock",
	"open_cover": "open", "close_cover": "close", "stop_cover": "stop",
	"set_cover_position": "position", "set_cover_tilt_position": "tilt",
	"toggle_cover_tilt": "tilt",
	"start_mowing":      "mow", "pause_mowing": "pause", "dock": "dock",
	"play_pause": "play/pause", "media_play": "play", "media_pause": "pause",
	"media_stop": "stop", "media_next_track": "next", "media_previous_track": "prev",
	"volume_up": "vol+", "volume_down": "vol-", "mute": "mute", "unmute": "unmute",
	"set_percentage": "level", "set_humidity": "humidity", "set_temperature": "temp",
	"return_to_base": "dock", "clean_spot": "spot", "locate": "locate",
	"take_snapshot": "snapshot",
}

func verbLabel(v string) string {
	if l, ok := verbLabels[v]; ok {
		return l
	}
	return v
}

// action is one service bound to one entity. order keeps the canonical verb
// ordering after the domain list is sorted.
type action struct {
	service string
	label   string
	order   int
}

// registry maps an entity domain to the actions offered for it.
type registry struct {
	byDomain map[string][]action
	// usable is false when the server advertised no target information at
	// all, which means we cannot tell actionable from read-only and should
	// show everything rather than show nothing.
	usable bool
}

// newRegistry reads the service list. Three filters apply:
//
//   - the service must take an entity, so global services like
//     script.start_bedroom_ac do not make every script look actionable;
//   - the name must be a known control verb, so sensors, weather and
//     calendars drop out even where they technically accept a target;
//   - it must need no required arguments, because we have nothing to pass and
//     the call would only fail.
func newRegistry(services []Service) registry {
	r := registry{byDomain: map[string][]action{}}
	seen := map[string]bool{}
	sawTarget := false

	for _, s := range services {
		targets := s.entityDomains()
		if len(targets) == 0 {
			continue
		}
		sawTarget = true
		order, isControl := verbOrder[s.Name]
		if !isControl {
			continue
		}
		if hasRequired(s.Fields) {
			continue
		}
		for _, d := range targets {
			key := d + "." + s.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			r.byDomain[d] = append(r.byDomain[d], action{
				service: s.Name,
				label:   verbLabel(s.Name),
				order:   order,
			})
		}
	}

	for _, acts := range r.byDomain {
		sort.Slice(acts, func(i, j int) bool { return acts[i].order < acts[j].order })
	}
	r.usable = sawTarget
	return r
}

func hasRequired(fields map[string]ServiceField) bool {
	for _, f := range fields {
		if f.Required {
			return true
		}
	}
	return false
}

// actions returns the ordered actions for a domain, or nil.
func (r registry) actions(d string) []action { return r.byDomain[d] }

// controllable reports whether the domain has at least one action.
func (r registry) controllable(d string) bool { return len(r.byDomain[d]) > 0 }

// find returns the action with the given service name, or nil.
func (r registry) find(d, service string) *action {
	for i, a := range r.byDomain[d] {
		if a.service == service {
			return &r.byDomain[d][i]
		}
	}
	return nil
}
