package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestPadAlignsByRunes(t *testing.T) {
	// The bug this fixes: %-13s pads by bytes, and the ellipsis is three
	// bytes, so every truncated value pulled its column left.
	long := "2026-10-16T13:32:27.049949+00:00"
	got := []int{
		len([]rune(pad(long, 13))),
		len([]rune(pad("off", 13))),
		len([]rune(pad("", 13))),
		len([]rune(pad("exactlythirteen", 13))),
		len([]rune(pad("onechar", 1))),
	}
	for i, n := range got {
		if n != 13 && i != 4 {
			t.Errorf("pad %d = %d runes, want 13 (%q)", i, n, pad(got2(i, long), 13))
		}
	}
	if p := pad("onechar", 1); p != "…" {
		t.Errorf("pad to 1 = %q", p)
	}
	if p := pad("ab", 5); p != "ab   " {
		t.Errorf("pad short = %q", p)
	}
}

func got2(i int, long string) string {
	return []string{long, "off", "", "exactlythirteen"}[i]
}

func TestUIConfigRank(t *testing.T) {
	c := uiConfig{GroupOrder: []string{"light", "lock"}}
	if rank(c.orderFor("groups"), "light") != 0 || rank(c.orderFor("groups"), "lock") != 1 {
		t.Errorf("ranks wrong: %d %d", rank(c.GroupOrder, "light"), rank(c.GroupOrder, "lock"))
	}
	// A domain added to the house since the order was written sorts below them.
	if rank(c.orderFor("groups"), "sensor") != len(c.GroupOrder) {
		t.Error("an unlisted domain should sort below every listed one")
	}
}

// Each view keeps its own order, in its own name space: an area order saved as
// a domain order would bury the domain one.
func TestUIConfigOrderPerView(t *testing.T) {
	c := uiConfig{GroupOrder: []string{"light"}}
	if got := c.orderFor("groups"); !slices.Equal(got, []string{"light"}) {
		t.Errorf("groups order = %v", got)
	}
	// The flat view has exactly one group, so it falls through to the domain
	// order harmlessly - there is nothing to reorder there anyway.
	if got := c.orderFor("flat"); !slices.Equal(got, []string{"light"}) {
		t.Errorf("flat order = %v", got)
	}

	// Writing one view's order must not disturb the others, or the next write
	// would quietly drop a field.
	next := c.withOrder("areas", []string{"Kitchen", "Hallway"})
	if !slices.Equal(next.orderFor("areas"), []string{"Kitchen", "Hallway"}) {
		t.Errorf("areas order = %v", next.AreaOrder)
	}
	if !slices.Equal(next.orderFor("groups"), []string{"light"}) {
		t.Errorf("the domain order was lost: %v", next.GroupOrder)
	}
	if len(c.AreaOrder) != 0 {
		t.Error("withOrder mutated the receiver")
	}
}

func TestUIConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HAS_CONFIG", filepath.Join(dir, "config.json"))

	want := uiConfig{GroupOrder: []string{"light", "cover"}}
	if err := saveUIConfig(want); err != nil {
		t.Fatal(err)
	}
	// No secrets in this file, so it need not be private.
	info, err := os.Stat(filepath.Join(dir, "ui.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("ui.json mode = %v", info.Mode().Perm())
	}
	got, err := loadUIConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip: got %+v want %+v", got, want)
	}

	// A missing file is not an error.
	t.Setenv("HAS_CONFIG", filepath.Join(dir, "nothing", "config.json"))
	if got, err := loadUIConfig(); err != nil || len(got.GroupOrder) != 0 {
		t.Errorf("missing file should give an empty config: %+v %v", got, err)
	}
}

// The UI writes the file, so the wiring has to hold together end to end.
// Reordering is on alt+arrows, because shift+arrow is what people press to
// scroll faster and it must not rewrite the saved order.
//
// Both directions, from a cold start: alt+down used to be impossible, because it
// swapped inside the saved *pin list*, which is empty on a fresh install. The
// group at the top of the screen answered "already bottom".
func TestUIReorderPersists(t *testing.T) {
	t.Setenv("HAS_CONFIG", filepath.Join(t.TempDir(), "config.json"))

	m := uiFixture() // displayed: light, switch; nothing saved yet
	m.resetCursor()
	if got := groupRows(m); !slices.Equal(got, []string{"light", "switch"}) {
		t.Fatalf("precondition: displayed order is %v", got)
	}
	on := func(header string) {
		t.Helper()
		for i, r := range m.rows() {
			if r.isGroup && r.domain == header {
				m.cursor = i
			}
		}
	}
	move := func(t *testing.T, delta int) {
		t.Helper()
		key := tea.KeyMsg{Type: tea.KeyDown, Alt: true}
		if delta < 0 {
			key = tea.KeyMsg{Type: tea.KeyUp, Alt: true}
		}
		mm, _ := m.Update(key)
		m = mm.(*uiModel)
	}

	on("light")
	move(t, 1)
	if got := groupRows(m); !slices.Equal(got, []string{"switch", "light"}) {
		t.Errorf("light should have moved down: %v", got)
	}
	if !strings.Contains(m.status, "moved light") {
		t.Errorf("status should confirm, got %q", m.status)
	}
	// The cursor stays on the group that moved.
	if r, _ := m.rowAt(m.cursor); !r.isGroup || r.domain != "light" {
		t.Errorf("cursor left the group it moved: %+v", r)
	}
	onDisk, err := loadUIConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk.GroupOrder) < 2 || !slices.Equal(onDisk.GroupOrder[:2], []string{"switch", "light"}) {
		t.Errorf("not written to ui.json: %v", onDisk.GroupOrder)
	}

	// And back up.
	on("light")
	move(t, -1)
	if got := groupRows(m); !slices.Equal(got, []string{"light", "switch"}) {
		t.Errorf("light should have moved back up: %v", got)
	}

	// The end of the list says so, and the right end this time.
	on("switch")
	move(t, 1)
	if !strings.Contains(m.status, "switch is already bottom") {
		t.Errorf("the last group is already bottom, got %q", m.status)
	}
}

// The order is saved whole, so reordering what is on screen cannot drop the
// groups that are not: sun.sun is hidden until ^A, and a partial order would
// forget it every time.
func TestUIReorderKeepsHiddenGroups(t *testing.T) {
	t.Setenv("HAS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	m := uiFixture() // sun.sun is read-only, so the list shows light and switch
	m.resetCursor()
	if got := groupRows(m); !slices.Equal(got, []string{"light", "switch"}) {
		t.Fatalf("precondition: %v", got)
	}
	m.cursor = 0 // the light header
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown, Alt: true})
	got := mm.(*uiModel)

	if len(got.cfg.GroupOrder) != 3 || got.cfg.GroupOrder[2] != "sun" {
		t.Errorf("the hidden domain should still be saved: %v", got.cfg.GroupOrder)
	}
	onDisk, err := loadUIConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk.GroupOrder) != 3 {
		t.Errorf("written to ui.json: %v", onDisk.GroupOrder)
	}
	// And with it revealed, the saved order is still the order on screen.
	got.showAll = true
	if g := groupRows(got); !slices.Equal(g, []string{"switch", "light", "sun"}) {
		t.Errorf("^A should show the same order: %v", g)
	}
}

// Every view that groups has an order worth keeping, and each keeps its own:
// reordering areas must not disturb the domain order, which shares a file with it
// and used to be the only order there was.
func TestUIReorderEveryView(t *testing.T) {
	t.Setenv("HAS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	m := uiFixture()
	m.areas = map[string]string{
		"light.kitchen_light": "Kitchen", "light.kitchen_light_2": "Kitchen",
		"light.garage_light": "Garage", "switch.coffee": "Kitchen",
	}
	m.cfg.GroupOrder = []string{"light", "switch"}

	move := func(t *testing.T, view, group string) *uiModel {
		t.Helper()
		m.setView(view)
		m.resetCursor()
		for i, r := range m.rows() {
			if r.isGroup && r.domain == group {
				m.cursor = i
			}
		}
		mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyDown, Alt: true})
		if cmd != nil {
			t.Error("nothing to refresh: the list did not change")
		}
		return mm.(*uiModel)
	}

	got := move(t, "areas", "Kitchen")
	if !strings.Contains(got.status, "moved Kitchen") {
		t.Errorf("status = %q", got.status)
	}
	if len(got.cfg.AreaOrder) == 0 || got.cfg.AreaOrder[0] != "Garage" {
		t.Errorf("the area order should have moved Kitchen down: %v", got.cfg.AreaOrder)
	}
	// The domain order is untouched: area names and domain names are different
	// things, and one file holds both.
	if !slices.Equal(got.cfg.GroupOrder, []string{"light", "switch"}) {
		t.Errorf("area names reached group_order: %v", got.cfg.GroupOrder)
	}
	onDisk, err := loadUIConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(onDisk.GroupOrder, []string{"light", "switch"}) {
		t.Errorf("written to ui.json: %v", onDisk.GroupOrder)
	}

	// The states view keeps its own as well, and comes back to the same place
	// in its own list.
	got = move(t, "states", "off")
	if len(got.cfg.StateOrder) == 0 {
		t.Errorf("the state order should have been written: %v", got.cfg.StateOrder)
	}
	if !slices.Equal(got.cfg.AreaOrder, []string{"Garage", "Kitchen", "unknown"}) {
		t.Errorf("the area order was disturbed: %v", got.cfg.AreaOrder)
	}
}

// Favorites are the one place a single entity has an order of its own, and it is
// the order they were pinned in.
func TestUIFavoritesReorder(t *testing.T) {
	t.Setenv("HAS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	m := uiFixture()
	m.cfg.Favorites = []string{"light.garage_light", "light.kitchen_light", "switch.coffee"}
	m.resetCursor()

	favs := func(m *uiModel) []string {
		t.Helper()
		var out []string
		for _, r := range m.rows() {
			if !r.isGroup && r.domain == favoritesGroup {
				out = append(out, r.entity.EntityID)
			}
		}
		return out
	}
	if got := favs(m); !slices.Equal(got, m.cfg.Favorites) {
		t.Fatalf("favorites should be listed in the saved order: %v", got)
	}
	on := func(m *uiModel, id string) *uiModel {
		t.Helper()
		// First match wins: a favorite is in the list twice, and the favorites
		// copy is the one at the top.
		for i, r := range m.rows() {
			if !r.isGroup && r.entity.EntityID == id {
				m.cursor = i
				return m
			}
		}
		t.Fatalf("%s is not in the list", id)
		return m
	}
	move := func(t *testing.T, m *uiModel, up bool) *uiModel {
		t.Helper()
		key := tea.KeyMsg{Type: tea.KeyDown, Alt: true}
		if up {
			key = tea.KeyMsg{Type: tea.KeyUp, Alt: true}
		}
		mm, _ := m.Update(key)
		return mm.(*uiModel)
	}

	// The last one, up one place.
	got := move(t, on(m, "switch.coffee"), true)
	if want := []string{"light.garage_light", "switch.coffee", "light.kitchen_light"}; !slices.Equal(favs(got), want) {
		t.Errorf("favorites should be %v, got %v", want, favs(got))
	}
	if !strings.Contains(got.status, "in favorites") {
		t.Errorf("status should name the order it changed, got %q", got.status)
	}
	onDisk, err := loadUIConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(onDisk.Favorites, favs(got)) {
		t.Errorf("not written to ui.json: %v", onDisk.Favorites)
	}
	// The cursor stayed on the favorite that moved.
	if r, _ := got.rowAt(got.cursor); r.entity.EntityID != "switch.coffee" {
		t.Errorf("cursor left the favorite it moved: %+v", r)
	}
	// The top cannot go higher.
	if got := move(t, on(got, "light.garage_light"), true); !strings.Contains(got.status, "already top") {
		t.Errorf("status = %q", got.status)
	}

	// An entity that is not a favorite has no order of its own to move within,
	// and says so rather than quietly moving its domain instead.
	got = on(got, "light.kitchen_light_2")
	got = move(t, got, true)
	if !strings.Contains(got.status, "only favorites") {
		t.Errorf("status = %q", got.status)
	}
}
