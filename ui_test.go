package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// testServices mirrors the shape real Home Assistant returns: grouped by
// domain, each service declaring which entity domains it can target.
func testServices() []Service {
	t := func(domains ...string) *ServiceTarget {
		t := &ServiceTarget{}
		for _, d := range domains {
			t.Entity = append(t.Entity, struct {
				Domain []string `json:"domain"`
			}{Domain: []string{d}})
		}
		return t
	}
	return []Service{
		{Domain: "light", Name: "turn_on", Target: t("light")},
		{Domain: "light", Name: "turn_off", Target: t("light")},
		{Domain: "light", Name: "toggle", Target: t("light")},
		{Domain: "switch", Name: "turn_on", Target: t("switch")},
		{Domain: "switch", Name: "turn_off", Target: t("switch")},
		{Domain: "switch", Name: "toggle", Target: t("switch")},
		{Domain: "lock", Name: "lock", Target: t("lock")},
		{Domain: "lock", Name: "unlock", Target: t("lock")},
		{Domain: "lock", Name: "open", Target: t("lock")},
		// Sensors have no control services at all, so they drop out.
		{Domain: "sensor", Name: "reset", Target: nil},
	}
}

// uiFixture starts fully expanded with a registry loaded; TestUIFoldedByDefault
// covers cmdUI's real default, which is collapsed.
func uiFixture() *uiModel {
	return &uiModel{
		client: &Client{base: "http://ha.local:8123"},
		states: []State{
			{EntityID: "light.kitchen_light", State: "on", Attributes: map[string]any{"friendly_name": "Kitchen Light"}},
			{EntityID: "light.kitchen_light_2", State: "off", Attributes: map[string]any{"friendly_name": "Kitchen Light 2"}},
			{EntityID: "light.garage_light", State: "off", Attributes: map[string]any{"friendly_name": "Garage Light"}},
			{EntityID: "switch.coffee", State: "off", Attributes: map[string]any{"friendly_name": "Coffee"}},
			{EntityID: "sun.sun", State: "above_horizon"},
		},
		reg:    newRegistry(testServices()),
		folded: map[string]bool{},
		height: defaultHeight,
		ready:  true,
	}
}

// uiFolded matches cmdUI's default: every group collapsed.
func uiFolded() *uiModel {
	m := uiFixture()
	m.defaultFolded = true
	return m
}

func typeRunes(m *uiModel, s string) *uiModel {
	// "/" enters search mode, only then can any printable reach the filter.
	for _, r := range "/" + s {
		var mm tea.Model = m
		mm, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = mm.(*uiModel)
	}
	return m
}

func press(m *uiModel, t tea.KeyType) (*uiModel, tea.Cmd) {
	var mm tea.Model = m
	var cmd tea.Cmd
	mm, cmd = mm.Update(tea.KeyMsg{Type: t})
	return mm.(*uiModel), cmd
}

func entityRows(m *uiModel) []string {
	var out []string
	for _, r := range m.rows() {
		if !r.isGroup {
			out = append(out, r.entity.EntityID)
		}
	}
	return out
}

func groupRows(m *uiModel) []string {
	var out []string
	for _, r := range m.rows() {
		if r.isGroup {
			out = append(out, r.domain)
		}
	}
	return out
}

// Folding is the default the user asked for: groups collapsed, entities hidden.
func TestUIFoldedByDefault(t *testing.T) {
	m := uiFolded()
	t.Log("\n" + m.View())

	if got := groupRows(m); len(got) != 2 {
		t.Errorf("expected 2 group headers (sun has no actions), got %v", got)
	}
	if got := entityRows(m); len(got) != 0 {
		t.Errorf("folded groups should hide their entities, got %v", got)
	}
	// Busy domains sort first.
	if groupRows(m)[0] != "light" {
		t.Errorf("groups should be ordered by size, got %v", groupRows(m))
	}
	for _, want := range []string{"light (3)", "1 on"} {
		if !strings.Contains(m.View(), want) {
			t.Errorf("folded view missing %q", want)
		}
	}
	// A domain with no actions is not offered at all.
	if strings.Contains(m.View(), "sun.sun") {
		t.Error("sun.sun has no actions and should be hidden by default")
	}
}

// ^A reveals the read-only entities again.
func TestUIShowAll(t *testing.T) {
	m := uiFixture()
	if strings.Contains(m.View(), "sun.sun") {
		t.Fatal("precondition: sun should be hidden")
	}
	m, _ = press(m, tea.KeyCtrlA)
	if !strings.Contains(m.View(), "sun.sun") {
		t.Errorf("^A should reveal read-only entities: %q", m.View())
	}

	// Acting on one of them is refused with a reason, not a crash.
	for i := range m.rows() {
		r, _ := m.rowAt(i)
		if r.isGroup || r.entity.EntityID != "sun.sun" {
			continue
		}
		m.cursor = i
		if cmd := m.runAction(0); cmd != nil {
			t.Error("read-only entity should not produce a command")
		}
		if !strings.Contains(m.status, "nothing to do") {
			t.Errorf("status should explain, got %q", m.status)
		}
	}
}

// Filtering searches every entity, including ones hidden from the list, so
// hiding read-only things never makes them unreachable.
func TestUIFilterFindsHidden(t *testing.T) {
	m := typeRunes(uiFixture(), "sun")
	if got := entityRows(m); len(got) != 1 || got[0] != "sun.sun" {
		t.Errorf("filtering should reach hidden entities, got %v", got)
	}
}

// Hiding (h) is the other "hidden": a saved preference that takes an entity
// out of the list, the same key brings it back, and H lists what is gone
// alone. Search reaches h-hidden rows too; the marker tells them apart from
// the rest.
func TestUIHideUnhide(t *testing.T) {
	t.Setenv("HAS_CONFIG", filepath.Join(t.TempDir(), "config.json"))

	m := uiFixture()
	on := func(t *testing.T, m *uiModel, id string) *uiModel {
		t.Helper()
		for i, r := range m.rows() {
			if !r.isGroup && r.entity.EntityID == id {
				m.cursor = i
				return m
			}
		}
		t.Fatalf("%s is not in the list", id)
		return m
	}

	// h on an entity: out of the list, into ui.json.
	got, _ := pressRune(on(t, m, "switch.coffee"), 'h')
	if !slices.Equal(got.cfg.Hidden, []string{"switch.coffee"}) {
		t.Fatalf("h should hide the highlighted entity, got %v", got.cfg.Hidden)
	}
	for _, r := range got.rows() {
		if r.entity.EntityID == "switch.coffee" {
			t.Errorf("the hidden entity is still in the list")
		}
	}
	if s := got.status; !strings.Contains(s, "hid switch.coffee") {
		t.Errorf("status should explain, got %q", s)
	}
	onDisk, err := loadUIConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(onDisk.Hidden, []string{"switch.coffee"}) {
		t.Errorf("not written to ui.json: %v", onDisk.Hidden)
	}

	// Search still reaches it, and says so on the row.
	got = typeRunes(got, "coffee")
	if ids := entityRows(got); !slices.Equal(ids, []string{"switch.coffee"}) {
		t.Fatalf("search should reach what h hid, got %v", ids)
	}
	if view := stripANSI(got.View()); !strings.Contains(view, "(hidden) Coffee") {
		t.Errorf("the row should say it is hidden, got:\n%s", view)
	}
	got.filter, got.searching = "", false

	// H lists only what is hidden, and h there puts it back.
	got, _ = pressRune(got, 'H')
	if ids := entityRows(got); !slices.Equal(ids, []string{"switch.coffee"}) {
		t.Fatalf("H should list what is hidden, got %v", ids)
	}
	if !got.expanded("switch") {
		t.Errorf("groups reveal in the hidden view, or it starts folded and unreadable")
	}
	got, _ = pressRune(on(t, got, "switch.coffee"), 'h')
	if ids := entityRows(got); !slices.Contains(ids, "switch.coffee") {
		t.Errorf("unhiding should put the entity back in the list, got %v", ids)
	}
	onDisk, _ = loadUIConfig()
	if len(onDisk.Hidden) != 0 {
		t.Errorf("unhiding should clear ui.json, got %v", onDisk.Hidden)
	}

	// H with nothing hidden says so; a group header has nothing to hide.
	empty, _ := pressRune(uiFixture(), 'H')
	if got := entityRows(empty); len(got) != 0 {
		t.Errorf("H over an empty hidden list should show nothing, got %v", got)
	}
	if s := empty.status; !strings.Contains(s, "nothing hidden") {
		t.Errorf("status should explain, got %q", s)
	}
	header := uiFixture()
	if !header.rows()[0].isGroup {
		t.Fatal("fixture should start on a group header")
	}
	header.cursor = 0
	after, _ := pressRune(header, 'h')
	if len(after.cfg.Hidden) != 0 {
		t.Errorf("h on a group header hid %v", after.cfg.Hidden)
	}
	if s := after.status; !strings.Contains(s, "pick an entity") {
		t.Errorf("status should refuse, got %q", s)
	}
}

// The cursor is held by identity, never by index: hide and unhide reshape the
// list under the cursor, so each press has to keep its bearings.
func TestUIHideCursor(t *testing.T) {
	t.Setenv("HAS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	on := func(t *testing.T, m *uiModel, id string) *uiModel {
		t.Helper()
		for i, r := range m.rows() {
			if !r.isGroup && r.entity.EntityID == id {
				m.cursor = i
				return m
			}
		}
		t.Fatalf("%s is not in the list", id)
		return m
	}
	at := func(t *testing.T, m *uiModel) string {
		t.Helper()
		r, ok := m.rowAt(m.cursor)
		if !ok {
			return "<none>"
		}
		if r.isGroup {
			return "header:" + r.domain
		}
		return r.entity.EntityID
	}
	// The fixture reads, top to bottom: light header, garage, kitchen,
	// kitchen_2, switch header, coffee.

	t.Run("hide the row under the cursor", func(t *testing.T) {
		m, _ := pressRune(on(t, uiFixture(), "light.kitchen_light"), 'h')
		// The row that shifted up into the cursor's index takes its place.
		if got := at(t, m); got != "light.kitchen_light_2" {
			t.Errorf("cursor should follow the list, got %s", got)
		}
	})

	t.Run("hide the last row of the last group", func(t *testing.T) {
		// Coffee leaves with the switch header behind it; the cursor clamps
		// onto an entity, not off the end.
		m, _ := pressRune(on(t, uiFixture(), "switch.coffee"), 'h')
		if got := at(t, m); got != "light.kitchen_light_2" {
			t.Errorf("cursor should land on the last entity, got %s", got)
		}
	})

	t.Run("unhide one of two, still in the hidden view", func(t *testing.T) {
		m := uiFixture()
		m, _ = pressRune(on(t, m, "light.kitchen_light"), 'h')
		m, _ = pressRune(on(t, m, "light.kitchen_light_2"), 'h')
		m, _ = pressRune(m, 'H')
		m, _ = pressRune(m, 'h') // kitchen_light goes back, kitchen_2 stays
		if !m.showHidden {
			t.Fatal("a hidden entity is left, the hidden view should stay")
		}
		if ids := entityRows(m); !slices.Equal(ids, []string{"light.kitchen_light_2"}) {
			t.Fatalf("the still-hidden entity should be listed, got %v", ids)
		}
		// The cursor holds its bearings inside the still-open hidden view.
		if got := at(t, m); got != "light.kitchen_light_2" {
			t.Errorf("cursor should stay in the hidden view, got %s", got)
		}
	})

	t.Run("unhide the last one, land in the normal list", func(t *testing.T) {
		m := uiFixture()
		m, _ = pressRune(on(t, m, "switch.coffee"), 'h')
		m, _ = pressRune(m, 'H')
		m, _ = pressRune(on(t, m, "switch.coffee"), 'h')
		if m.showHidden {
			t.Error("the hidden view should empty itself out")
		}
		if got := at(t, m); got != "switch.coffee" {
			t.Errorf("the cursor should hold the unhidden entity, got %s", got)
		}
	})
}

func TestUIFoldToggle(t *testing.T) {
	m := uiFolded()
	if len(entityRows(m)) != 0 {
		t.Fatal("expected a fully folded start")
	}
	// → opens the highlighted group, ← closes it. They are the only keys that
	// touch one group; tab is for all of them.
	m, _ = press(m, tea.KeyRight)
	if len(entityRows(m)) != 3 {
		t.Errorf("→ should expand the highlighted group, got %v", entityRows(m))
	}
	m, _ = press(m, tea.KeyLeft)
	if len(entityRows(m)) != 0 {
		t.Errorf("← should collapse again, got %v", entityRows(m))
	}
}

// esc folds but never unfolds: pressing it repeatedly walks down the list and
// leaves everything closed, which is the one thing a fold-all key could do and
// the reason tab is gone.
func TestUIEscFoldsDownAndNeverUnfolds(t *testing.T) {
	m := uiFolded()
	m, _ = press(m, tea.KeyRight) // open exactly one group
	if len(entityRows(m)) != 3 {
		t.Fatalf("expected one open group, got %v", entityRows(m))
	}

	for i := 0; i < 10; i++ {
		m, _ = press(m, tea.KeyEsc)
	}
	if got := entityRows(m); len(got) != 0 {
		t.Errorf("repeated esc should end with everything folded, left open: %v", got)
	}
	if got := groupRows(m); len(got) != 2 {
		t.Errorf("both group headers should still be listed, got %v", got)
	}

	// And it stays shut: esc is not a toggle in disguise.
	m2 := uiFixture()
	m2.resetCursor()
	before := len(entityRows(m2))
	m2, _ = press(m2, tea.KeyEsc)
	m2, _ = press(m2, tea.KeyEsc)
	if len(entityRows(m2)) >= before {
		t.Errorf("esc must never unfold: %d entities before, %d after", before, len(entityRows(m2)))
	}
}

// tab is unbound now. It must not fold, and it must not become filter text
// either: a key that does something surprising is worse than one that does
// nothing.
func TestUITabDoesNothing(t *testing.T) {
	m := uiFolded()
	m, _ = press(m, tea.KeyRight)
	before := len(entityRows(m))
	got, _ := press(m, tea.KeyTab)
	if len(entityRows(got)) != before {
		t.Errorf("tab should do nothing, entities %d -> %d", before, len(entityRows(got)))
	}
	if got.filter != "" {
		t.Errorf("tab must not become filter text, got %q", got.filter)
	}
}

func TestUIFilterRevealsGroups(t *testing.T) {
	// A folded list must still show matches when filtering, otherwise a
	// search would return nothing but headers.
	m := typeRunes(uiFolded(), "coffee")
	if got := entityRows(m); len(got) != 1 || got[0] != "switch.coffee" {
		t.Errorf("filtering a folded tree should reveal matches, got %v", got)
	}
	t.Log("\n" + m.View())

	// A group name works the same way.
	m = typeRunes(uiFolded(), "light")
	if got := entityRows(m); len(got) != 3 {
		t.Errorf("group filter matched %d, want 3", len(got))
	}
	// ...and only matching groups get headers.
	if got := groupRows(m); len(got) != 1 || got[0] != "light" {
		t.Errorf("got %v", got)
	}
}

func TestUIScreen(t *testing.T) {
	m := uiFixture()
	t.Log("\n" + m.View())
	for _, want := range []string{"http://ha.local:8123", "Kitchen Light", "switch.coffee"} {
		if !strings.Contains(m.View(), want) {
			t.Errorf("view missing %q", want)
		}
	}
}

// The selected row advertises that it has actions, and only that row does.
func TestUIActionHints(t *testing.T) {
	m := uiFixture()
	m.resetCursor() // first entity row: a light
	view := m.View()
	if !strings.Contains(view, "3 acts") {
		t.Errorf("selected row should advertise its actions:\n%s", view)
	}
	if n := strings.Count(view, "3 acts"); n != 1 {
		t.Errorf("only the selected row should advertise actions, got %d", n)
	}
	// Space is the first action.
	if got := m.reg.actions("light"); len(got) != 3 ||
		got[0].service != "toggle" || got[1].service != "turn_on" || got[2].service != "turn_off" {
		t.Errorf("light actions = %+v", got)
	}
}

// enter opens a sub-menu instead of firing an action directly, and the
// selection it runs must survive the menu closing.
func TestUISubMenu(t *testing.T) {
	m := uiFixture()
	m.resetCursor()

	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Error("opening the menu should not fire an action")
	}
	got := mm.(*uiModel)
	if !got.menu || got.menuIdx != 0 {
		t.Fatalf("menu should be open at the first action: %+v", got)
	}
	for _, want := range []string{"toggle", "turn_on", "turn_off", "choose"} {
		if !strings.Contains(got.View(), want) {
			t.Errorf("menu missing %q:\n%s", want, got.View())
		}
	}
	// The sub-menu belongs to the highlighted row and renders directly
	// beneath it, not in place of it.
	view := got.View()
	rowAt := strings.Index(view, "Garage Light")
	menuAt := strings.Index(view, "actions for")
	if rowAt < 0 || menuAt < 0 || menuAt < rowAt {
		t.Errorf("menu should follow its row:\n%s", view)
	}
	t.Log("\n" + view)

	// Arrows move within the menu, not the list.
	before := got.cursor
	got, _ = press(got, tea.KeyDown)
	if got.menuIdx != 1 {
		t.Errorf("down should select the second action, got %d", got.menuIdx)
	}
	if got.cursor != before {
		t.Error("the list cursor must not move while the menu is open")
	}
	got, _ = press(got, tea.KeyUp)
	got, _ = press(got, tea.KeyUp) // clamp
	if got.menuIdx != 0 {
		t.Errorf("up should clamp at 0, got %d", got.menuIdx)
	}

	// Pick the second action and run it: this is the case that would
	// silently run the first one if closing reset the index.
	got, _ = press(got, tea.KeyDown)
	if got.menuIdx != 1 {
		t.Fatalf("expected the second action, got %d", got.menuIdx)
	}
	mm, cmd = got.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Error("enter in the menu should run the chosen action")
	}
	if !mm.(*uiModel).menu {
		t.Error("the menu, per the new behaviour, stays open so the choice persists")
	}
	if !strings.Contains(mm.(*uiModel).status, "on light.garage_light") {
		t.Errorf("should have run turn_on, got status %q", mm.(*uiModel).status)
	}

	// esc closes without acting.
	reopen := uiFixture()
	reopen.resetCursor()
	mm, _ = reopen.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !mm.(*uiModel).menu {
		t.Fatal("setup: menu should be open")
	}
	mm, cmd = mm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Error("esc should close the menu without acting")
	}
	if mm.(*uiModel).menu {
		t.Error("esc should close the menu")
	}
}

// With the sub-menu open, the menu owns the keyboard: a printable key must not
// extend the filter and move the cursor out from under an action about to run.
// Digits are the exception — they run a menu entry directly.
func TestUIMenuOwnsTheKeyboard(t *testing.T) {
	m := uiFixture()
	m.resetCursor()
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !mm.(*uiModel).menu {
		t.Fatal("setup: menu should be open")
	}
	before := mm.(*uiModel).selected().EntityID

	// A letter and a symbol are swallowed, not filtered.
	for _, r := range []string{"x", "!", "7"} {
		got := typeRunes(mm.(*uiModel), r)
		if got.filter != "" {
			t.Errorf("typing %q with the menu open should not extend the filter, got %q", r, got.filter)
		}
		if got.selected().EntityID != before {
			t.Errorf("typing %q moved the selection from %s to %s", r, before, got.selected().EntityID)
		}
		if !got.menu {
			t.Errorf("typing %q should leave the menu open", r)
		}
	}
}

// Digits 1-9 run a menu entry directly. A media_player has ten actions; arrows
// alone would take ten presses to reach the last one.
func TestUIMenuDigitsRunActions(t *testing.T) {
	m := uiFixture()
	m.resetCursor()
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := mm.(*uiModel)
	if !got.menu {
		t.Fatal("setup: menu should be open")
	}
	// turn_on is the second action in the registry (order: toggle, turn_on, turn_off).
	mm2, cmd := got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	if cmd == nil {
		t.Fatal("digit 2 in the menu should run the second action")
	}
	if !mm2.(*uiModel).menu {
		t.Error("the menu should stay open after running")
	}
	if !strings.Contains(mm2.(*uiModel).status, "on light.garage_light") {
		t.Errorf("should have run turn_on, got status %q", mm2.(*uiModel).status)
	}
}

// The sub-menu offers a favorite entry last, so running entry n has to use the
// list the menu drew. It read the registry instead, so the last entry - the one
// the README documents - died on "light has 3 actions, 4 is not one".
func TestUIMenuRunsTheFavoriteEntry(t *testing.T) {
	m := uiFixture()
	m.resetCursor()
	t.Setenv("HAS_CONFIG", filepath.Join(t.TempDir(), "config.json"))

	acts := m.actionsFor(m.cursor)
	if len(acts) == 0 || !acts[len(acts)-1].local {
		t.Fatalf("fixture drift: no trailing favorite entry in %v", acts)
	}

	// Its digit, which is one past the registry actions.
	digit := []rune{rune('0' + len(acts))}
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // open the menu
	got := mm.(*uiModel)
	if !got.menu {
		t.Fatal("menu should be open")
	}
	mm, _ = got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: digit})
	after := mm.(*uiModel)
	if !after.cfg.isFavorite(after.selected().EntityID) {
		t.Errorf("digit %q should have favorited, favorites=%v status=%q",
			digit, after.cfg.Favorites, after.status)
	}
	if strings.Contains(after.status, "is not one") {
		t.Errorf("the favorite entry reported itself missing: %q", after.status)
	}
	// The same entry, reached with enter instead of a digit.
	mm, _ = after.Update(tea.KeyMsg{Type: tea.KeyEnter})
	reopened := mm.(*uiModel)
	for reopened.menuIdx != len(acts)-1 {
		reopened.menuIdx++
	}
	mm, _ = reopened.Update(tea.KeyMsg{Type: tea.KeyEnter})
	final := mm.(*uiModel)
	if final.cfg.isFavorite(final.selected().EntityID) {
		t.Error("enter on the favorite entry should unfavorite it again")
	}
}

// Digits and symbols are free to type now that actions live in a sub-menu.
// Underscores and hyphens matter: they are everywhere in entity ids.
func TestUIDigitsAlwaysType(t *testing.T) {
	for _, word := range []string{"light2", "42", "a1b2", "light.hallway_light", "a-b", "+x"} {
		if got := typeRunes(uiFolded(), word).filter; got != word {
			t.Errorf("typed %q, filter is %q", word, got)
		}
	}
}

func TestUIEmptyAndLoading(t *testing.T) {
	m := &uiModel{client: &Client{base: "x"}, height: defaultHeight, ready: true}
	if !strings.Contains(m.View(), "no matches") {
		t.Errorf("empty filter should say so: %q", m.View())
	}
	m.ready = false
	if !strings.Contains(m.View(), "loading") {
		t.Errorf("before the first fetch it should say loading: %q", m.View())
	}
}

// Letters are filter text, never commands.
func TestUILettersAreFilterText(t *testing.T) {
	for _, word := range []string{"kitchen", "quj", "rug", "jfk", "coffee"} {
		if got := typeRunes(uiFolded(), word).filter; got != word {
			t.Errorf("typed %q, filter is %q", word, got)
		}
	}
	// Typing parks the cursor on the first matching entity, so space works
	// straight away rather than on a group header.
	m := typeRunes(uiFolded(), "kitchen")
	if got := m.selected().EntityID; got != "light.kitchen_light" {
		t.Errorf("after typing, selection should be the first match, got %q", got)
	}

	// Backspace removes one rune; ^U clears everything.
	m, _ = press(m, tea.KeyBackspace)
	if m.filter != "kitche" {
		t.Errorf("backspace left filter=%q, want kitche", m.filter)
	}
	m, _ = press(m, tea.KeyCtrlU)
	if m.filter != "" {
		t.Errorf("^U should clear the filter, got %q", m.filter)
	}

	// Backspace must not split a multi-byte rune.
	m = typeRunes(uiFolded(), "מטבח")
	m, _ = press(m, tea.KeyBackspace)
	if m.filter != "מטב" {
		t.Errorf("backspace on a multi-byte filter gave %q", m.filter)
	}
}

func TestUIActionKeys(t *testing.T) {
	m := uiFixture()
	m.resetCursor()
	// Space is printable and goes into the filter now; it must never act — the
	// primary action is the first entry in the sub-menu.
	mm := typeRunes(m, " ")
	if mm.filter != " " {
		t.Errorf("space should extend the filter, got %q", mm.filter)
	}
	// Enter opens the sub-menu, where space / `1` runs the primary action.
	m2 := uiFixture()
	m2.resetCursor()
	gm, _ := m2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !gm.(*uiModel).menu {
		t.Error("enter should open the sub-menu")
	}
	// On a group header there is nothing to act on, and the hint must not name
	// a key that does nothing: tab was unbound when fold-all went away.
	folded := uiFolded()
	if cmd := folded.runAction(0); cmd != nil {
		t.Error("acting on a folded group header should not fire a service")
	}
	if !strings.Contains(folded.status, "→ expands light") {
		t.Errorf("hint should say right expands, got %q", folded.status)
	}
	expanded := uiFixture()
	expanded.cursor = 0 // the light group header
	if cmd := expanded.runAction(0); cmd != nil {
		t.Error("acting on an expanded group header should not fire a service")
	}
	if !strings.Contains(expanded.status, "← folds light") {
		t.Errorf("hint should say left folds, got %q", expanded.status)
	}
}

func TestUINavigation(t *testing.T) {
	// After the first load the cursor sits on a real entity, so space works
	// immediately without an arrow press.
	m := uiFixture()
	m.resetCursor()
	if m.selected().EntityID != "light.garage_light" {
		t.Errorf("cursor should start on the first entity, got %q", m.selected().EntityID)
	}
	m, _ = press(m, tea.KeyDown)
	if m.selected().EntityID != "light.kitchen_light" {
		t.Errorf("down moved to %q", m.selected().EntityID)
	}
	m, _ = press(m, tea.KeyUp)
	if m.selected().EntityID != "light.garage_light" {
		t.Errorf("up moved to %q", m.selected().EntityID)
	}
	// The cursor must not run off either end.
	for i := 0; i < 99; i++ {
		m, _ = press(m, tea.KeyDown)
	}
	if m.cursor != len(m.rows())-1 {
		t.Errorf("cursor should clamp at the last row, got %d of %d", m.cursor, len(m.rows()))
	}
	for i := 0; i < 99; i++ {
		m, _ = press(m, tea.KeyUp)
	}
	if m.cursor != 0 {
		t.Errorf("cursor should clamp at 0, got %d", m.cursor)
	}
	// Typing resets the cursor onto the first match.
	m = typeRunes(m, "coffee")
	if got := m.selected().EntityID; got != "switch.coffee" {
		t.Errorf("filter should reset the selection, got %q", got)
	}
}

func TestUIScrolling(t *testing.T) {
	// A tall group must not run off the bottom, and the group must stay
	// visible once its header has scrolled away.
	m := &uiModel{
		client: &Client{base: "x"},
		height: 12,
		ready:  true,
		folded: map[string]bool{},
	}
	for i := 0; i < 200; i++ {
		m.states = append(m.states, State{
			EntityID:   fmt.Sprintf("sensor.s%03d", i),
			State:      "off",
			Attributes: map[string]any{"friendly_name": fmt.Sprintf("Sensor %d", i)},
		})
	}
	m, _ = press(m, tea.KeyDown) // step onto the sensor group header
	m, _ = press(m, tea.KeyDown) // into its entities
	for i := 0; i < 60; i++ {
		m, _ = press(m, tea.KeyDown)
	}
	view := m.View()
	t.Log("\n" + view)

	if !strings.Contains(view, "sensor") {
		t.Error("scrolled deep into a group, the group header should still be visible")
	}
	// The rows actually drawn must fit the available height.
	if n := strings.Count(view, "\n"); n > m.height+2 {
		t.Errorf("rendered %d lines into a %d-row terminal", n, m.height)
	}
}

// Navigation has to work without reaching for a mouse, and the arrow keys are
// the only ones that can be bound: letters and symbols are filter text.
func TestUINavigationKeys(t *testing.T) {
	t.Run("left and right fold", func(t *testing.T) {
		m := uiFixture()
		m.resetCursor() // light.garage_light, inside an expanded light group
		if r, _ := m.rowAt(m.cursor); r.isGroup {
			t.Fatal("expected an entity row")
		}
		m, _ = press(m, tea.KeyLeft)
		if got := entityRows(m); len(got) != 1 || got[0] != "switch.coffee" {
			t.Errorf("left should fold the group, entities left: %v", got)
		}
		m, _ = press(m, tea.KeyRight)
		if got := entityRows(m); len(got) != 4 {
			t.Errorf("right should unfold it, entities: %v", got)
		}
	})

	t.Run("page keys move a screenful", func(t *testing.T) {
		m := &uiModel{
			client: &Client{base: "x"}, height: 20, ready: true,
			folded: map[string]bool{}, reg: newRegistry(testServices()),
		}
		for i := 0; i < 200; i++ {
			m.states = append(m.states, State{
				EntityID: fmt.Sprintf("light.b%03d", i), State: "off",
				Attributes: map[string]any{"friendly_name": "Bulk"},
			})
		}
		m.resetCursor()
		one := m.cursor
		for _, k := range []tea.KeyType{tea.KeyPgDown, tea.KeyCtrlF, tea.KeyShiftDown} {
			m, _ = press(m, k)
			if m.cursor <= one {
				t.Fatalf("key %v should move down a screenful, %d -> %d", k, one, m.cursor)
			}
		}
		down := m.cursor
		for _, k := range []tea.KeyType{tea.KeyPgUp, tea.KeyCtrlB, tea.KeyShiftUp} {
			m, _ = press(m, k)
			if m.cursor >= down {
				t.Fatalf("key %v should move up a screenful, %d -> %d", k, down, m.cursor)
			}
		}
	})

	t.Run("home and end land on entities", func(t *testing.T) {
		m := uiFixture()
		first, last := -1, -1
		for i, r := range m.rows() {
			if r.isGroup {
				continue
			}
			if first < 0 {
				first = i
			}
			last = i
		}
		m, _ = press(m, tea.KeyEnd)
		if m.cursor != last {
			t.Errorf("end should land on the last entity row, got %d, want %d", m.cursor, last)
		}
		m, _ = press(m, tea.KeyHome)
		if m.cursor != first {
			t.Errorf("home should land on the first entity row, got %d, want %d", m.cursor, first)
		}
	})

	// esc is how you get out of a search. Quitting out from under a filter
	// throws away everything the user typed to get there.
	t.Run("esc goes back, and never quits", func(t *testing.T) {
		m := typeRunes(uiFixture(), "coffee")
		if m.filter != "coffee" {
			t.Fatalf("filter = %q", m.filter)
		}
		got, cmd := press(m, tea.KeyEsc)
		if got.filter != "" {
			t.Errorf("esc should clear the filter, got %q", got.filter)
		}
		if cmd == nil {
			t.Fatal("clearing the filter should still refresh the row")
		}
		if _, ok := cmd().(hoverMsg); !ok {
			t.Error("that command should be the hover refresh, not a quit")
		}
		// Nothing left to go back from: it folds, and only ever folds.
		before := len(entityRows(got))
		got, _ = press(got, tea.KeyEsc)
		if len(entityRows(got)) >= before {
			t.Errorf("esc should fold the group under the cursor")
		}
		// Still running: a letter still goes into the filter.
		if still := typeRunes(got, "x"); still.filter != "x" {
			t.Error("esc must not quit; ^C is the only way out")
		}
	})

	t.Run("click selects the row under the pointer", func(t *testing.T) {
		m := uiFixture()
		// The mapping is derived from the model, not from the last View: a click
		// no longer depends on a draw having happened first.
		f := m.frame(m.rows())
		// MouseMsg.Y counts from 1 and rowTop counts the lines above the list, so
		// the first drawn row arrives as rowTop+1. Selecting row start+1 here is
		// how every click landed a row below what was under the pointer.
		click := func(y int) *uiModel {
			return apply(m, tea.MouseMsg{
				Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
				X: 2, Y: y,
			})
		}
		if got := click(f.rowTop + 1); got.cursor != f.start {
			t.Errorf("click on the first drawn row landed on %d, want %d", got.cursor, f.start)
		}
		if got := click(f.rowTop + 2); got.cursor != f.start+1 {
			t.Errorf("click on the second drawn row landed on %d, want %d", got.cursor, f.start+1)
		}
		// The wheel scrolls instead of selecting.
		got := click(f.rowTop + 1)
		before := got.cursor
		got = apply(got, tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
		if got.cursor <= before {
			t.Errorf("wheel down should move the cursor, %d -> %d", before, got.cursor)
		}
		// A click above the list is not a row.
		outside := apply(got, tea.MouseMsg{
			Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
			X: 2, Y: 1, // the title bar
		})
		if outside.cursor != got.cursor {
			t.Error("clicking the title should not move the selection")
		}
	})
}

// Every run starts closed, whatever the last one was doing: a saved fold state
// made the list come back at 387 rows. Folding is a per-session thing; only the
// group order is remembered.
func TestUIStartsFoldedEveryRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HAS_CONFIG", filepath.Join(dir, "config.json"))

	// Leave ui.json in the state an older run would have written.
	if err := saveUIConfig(uiConfig{GroupOrder: []string{"light"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadUIConfig(); err != nil {
		t.Fatal(err)
	}

	m := uiFolded() // what cmdUI builds: defaultFolded, no restored map
	for _, d := range []string{"light", "switch"} {
		if m.expanded(d) {
			t.Errorf("%s should start folded", d)
		}
	}
	if got := len(m.rows()); got != len(groupRows(m)) {
		t.Errorf("no entities should be listed at all, got %d rows", got)
	}

	// --expand is the way to ask for the opposite.
	open := &uiModel{defaultFolded: false, folded: map[string]bool{}}
	if open.isFolded("light") {
		t.Error("--expand should start everything open")
	}
}

// ^D shows what the server actually knows about the highlighted entity. It is a
// control key on purpose: "?" is a printable rune and would have to go into the
// filter like any other character.
func TestUIDetails(t *testing.T) {
	m := uiFixture()
	m.resetCursor() // light.garage_light
	m.states[2].Attributes = map[string]any{
		"friendly_name":         "Garage Light",
		"battery_level":         42.0,
		"brightness":            128,
		"supported_color_modes": []any{"hs", "rgb"},
		"icon":                  "mdi:lightbulb",
	}
	m.cursor = 0
	m, _ = press(m, tea.KeyDown) // onto garage_light

	off := apply(m, tea.KeyMsg{Type: tea.KeyCtrlD})
	if !off.details {
		t.Fatal("ctrl+d should open the details")
	}
	view := off.View()
	t.Log("\n" + view)
	// Battery is hoisted to the top and rendered as a percentage, not as a raw
	// attribute buried in the dump.
	if i, j := strings.Index(view, "battery"), strings.Index(view, "brightness"); i < 0 || (j >= 0 && j < i) {
		t.Errorf("battery should lead the panel:\n%s", view)
	}
	for _, want := range []string{"42%", "brightness", "128", `["hs","rgb"]`} {
		if !strings.Contains(view, want) {
			t.Errorf("details missing %q:\n%s", want, view)
		}
	}
	// Nothing that says nothing.
	for _, noise := range []string{"friendly_name", "mdi:lightbulb", "battery_level"} {
		if strings.Contains(view, noise) {
			t.Errorf("details should skip %q:\n%s", noise, view)
		}
	}
	// It costs the list its rows, so the header must still be there.
	if !strings.Contains(view, "ha.local:8123") {
		t.Errorf("the frame must survive an open panel:\n%s", view)
	}
	if n := strings.Count(view, "\n"); n > off.height {
		t.Errorf("details pushed the view to %d lines in a %d-line terminal", n+1, off.height)
	}

	// ^D again, or esc, closes it.
	if again := apply(off, tea.KeyMsg{Type: tea.KeyCtrlD}); again.details {
		t.Error("ctrl+d should toggle the details off")
	}
	if closed := apply(off, tea.KeyMsg{Type: tea.KeyEsc}); closed.details {
		t.Error("esc should close the details")
	}
}

// An entity with fifty attributes must not push the header off the screen: the
// panel is trimmed to what the list can spare and says how much it left out.
func TestUIDetailsTrimsToFit(t *testing.T) {
	m := uiFixture()
	m.resetCursor()
	sel := m.selected().EntityID
	for i := range m.states {
		if m.states[i].EntityID != sel {
			continue
		}
		for j := 0; j < 50; j++ {
			m.states[i].Attributes[fmt.Sprintf("attr_%02d", j)] = j
		}
	}
	m.details = true
	lines := m.detailLines()
	if len(lines) > m.listHeight()+2 {
		t.Errorf("panel is %d lines for a %d-line list", len(lines), m.listHeight())
	}
	if !strings.Contains(lines[len(lines)-2], "more attributes") {
		t.Errorf("a trimmed panel should say how much it left out: %q", lines[len(lines)-2])
	}
	if n := strings.Count(m.View(), "\n"); n > m.height {
		t.Errorf("view is %d lines in a %d-line terminal", n+1, m.height)
	}
}

func TestWindow(t *testing.T) {
	cases := []struct{ cursor, n, h, wantStart, wantEnd int }{
		{0, 5, 10, 0, 5},       // everything fits
		{0, 100, 10, 0, 10},    // at the top
		{99, 100, 10, 90, 100}, // at the bottom
		{50, 100, 10, 45, 55},  // centred
		{0, 100, 1, 0, 1},      // degenerate height
	}
	for _, c := range cases {
		s, e := window(c.cursor, c.n, c.h)
		if s != c.wantStart || e != c.wantEnd {
			t.Errorf("window(%d,%d,%d) = %d,%d want %d,%d", c.cursor, c.n, c.h, s, e, c.wantStart, c.wantEnd)
		}
	}
}

func TestUIQuitKeys(t *testing.T) {
	// ^C is the only way out. esc is a "go back" key that folds when there is
	// nothing to go back from, and q is a letter, so it filters.
	var mm tea.Model = uiFixture()
	if _, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Error("ctrl+c should quit")
	}
	mm = uiFixture()
	// Still running: a letter still goes into the filter (once in search mode).
	mm, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	mm, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if mm.(*uiModel).filter != "x" {
		t.Error("esc must not quit; ^C is the only way out")
	}
	mm = uiFixture()
	// q is filter text again, once slash enters search mode.
	mm, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	_, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd != nil {
		t.Error("q must not quit: it is filter text")
	}
	if mm.(*uiModel).filter != "q" {
		t.Errorf("q should filter, got %q", mm.(*uiModel).filter)
	}
}

func TestUIStatusOnError(t *testing.T) {
	var mm tea.Model = uiFixture()
	mm, _ = mm.Update(fetchMsg{err: errNotConfigured})
	if !strings.Contains(mm.View(), "no server configured") {
		t.Errorf("a failed fetch should surface: %q", mm.View())
	}
	if mm.(*uiModel).kind != "err" {
		t.Errorf("kind = %q, want err", mm.(*uiModel).kind)
	}
}

func TestUITogglePatchesInPlace(t *testing.T) {
	// A toggle must not re-read every entity; it patches just the one it
	// touched, which is the whole point of the lazy fetch.
	m := uiFixture()
	var mm tea.Model = m
	on := State{EntityID: "light.garage_light", State: "on",
		Attributes: map[string]any{"friendly_name": "Garage Light"}}
	_, cmd := mm.Update(toggleMsg{entity: "light.garage_light", state: &on})
	if cmd != nil {
		t.Error("a toggle should not trigger a full refetch")
	}
	got := mm.(*uiModel)
	for _, s := range got.states {
		if s.EntityID != "light.garage_light" {
			continue
		}
		if s.State != "on" {
			t.Errorf("state not patched, got %q", s.State)
		}
		// The row's name comes from friendly_name, so a patch that dropped the
		// attributes left a nameless row with the id pushed into the name
		// column - and nothing re-read it until ^R on the polling path.
		if friendlyName(&s) != "Garage Light" {
			t.Errorf("patch dropped the name, got %q", friendlyName(&s))
		}
	}
	if !strings.Contains(got.status, "light.garage_light") {
		t.Errorf("status should name the entity, got %q", got.status)
	}
}

// A service call answers with every state it changed, and a light group is one
// of them: the group can come back before the member that was acted on.
func TestUIActionTakesTheEntityItActedOn(t *testing.T) {
	c := testServer(t, `[
		{"entity_id":"light.kitchen","state":"on","attributes":{"friendly_name":"Kitchen"}},
		{"entity_id":"light.kitchen_light","state":"on","attributes":{"friendly_name":"Kitchen Light"}}]`)

	msg, ok := actionCmd(c, "light.kitchen_light", "light", "turn_on")().(toggleMsg)
	if !ok {
		t.Fatal("actionCmd did not return a toggleMsg")
	}
	if msg.state == nil {
		t.Fatal("no state came back")
	}
	if msg.state.EntityID != "light.kitchen_light" {
		t.Errorf("took the group's state instead of the member's: %s = %q",
			msg.state.EntityID, msg.state.State)
	}
	if friendlyName(msg.state) != "Kitchen Light" {
		t.Errorf("the patch should carry the attributes, got name %q", friendlyName(msg.state))
	}
}

// A patch that arrives without attributes keeps the ones we already have, so a
// partial update can never blank a name.
func TestUIPatchKeepsAttributes(t *testing.T) {
	m := uiFixture()
	m.patch(State{EntityID: "light.garage_light", State: "on"})
	s := m.selected()
	if friendlyName(&s) != "" && friendlyName(&s) != "Garage Light" {
		t.Fatalf("fixture drift: name is %q", friendlyName(&s))
	}
	for _, st := range m.states {
		if st.EntityID == "light.garage_light" && friendlyName(&st) == "" {
			t.Error("a bare patch blanked the name")
		}
	}
}

// Moving the cursor must refresh only the highlighted entity, never the whole
// list, and stale hover requests must be dropped rather than applied.
func TestUILazyHoverFetch(t *testing.T) {
	m := uiFixture()
	m.resetCursor()

	// Moving schedules a debounced single-entity read.
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if cmd == nil {
		t.Fatal("moving should schedule a refresh")
	}
	first := mm.(*uiModel)

	// A superseded hover does nothing.
	gen := first.gen
	mm2, cmd2 := first.Update(hoverMsg{gen: gen - 1, entity: "light.kitchen_light"})
	if cmd2 != nil {
		t.Error("a superseded hover must not fetch")
	}
	if mm2.(*uiModel).gen != first.gen {
		t.Error("generation should be unchanged")
	}

	// The live hover issues exactly one single-entity read.
	mm3, cmd3 := first.Update(hoverMsg{gen: gen, entity: "light.kitchen_light"})
	if cmd3 == nil {
		t.Fatal("the live hover should issue a read")
	}
	// The response patches one entity and then stops; no further work chains.
	mm4, cmd4 := mm3.Update(oneMsg{
		gen:   mm3.(*uiModel).gen,
		state: State{EntityID: "light.kitchen_light", State: "CHANGED"},
	})
	if cmd4 != nil {
		t.Error("applying a fetch result should not chain another command")
	}

	// The highlighted entity is the one that changed.
	for _, s := range mm4.(*uiModel).states {
		if s.EntityID == "light.kitchen_light" && s.State != "CHANGED" {
			t.Errorf("hovered entity not patched: %q", s.State)
		}
	}

	// A stale response is ignored so an out-of-order reply cannot rewind a
	// value we have already moved past.
	mm5, _ := mm4.Update(oneMsg{
		gen:   gen - 1,
		state: State{EntityID: "light.kitchen_light", State: "STALE"},
	})
	for _, s := range mm5.(*uiModel).states {
		if s.EntityID == "light.kitchen_light" && s.State == "STALE" {
			t.Error("a stale fetch was applied")
		}
	}
}

// bubbletea's renderer keeps the last m.height lines of a view that overflows,
// so a reserve that is a couple of lines short does not clip the help line - it
// scrolls the title and the filter line off the top and shifts the list under
// the cursor. The sub-menu draws inside the window, so it counts too.
func TestUIViewFitsTerminal(t *testing.T) {
	m := uiFixture()
	for i := 0; i < 40; i++ {
		m.states = append(m.states, State{
			EntityID:   fmt.Sprintf("light.bulk_%02d", i),
			State:      "off",
			Attributes: map[string]any{"friendly_name": fmt.Sprintf("Bulk %02d", i)},
		})
	}
	m.setStatus("light.kitchen_light → on", statusOK)
	m.cursor = 20 // deep enough that the group indicator and "… more" are drawn

	fit := func(what string) {
		view := m.View()
		lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
		if len(lines) > m.height {
			t.Errorf("%s: view is %d lines for a %d-line terminal:\n%s",
				what, len(lines), m.height, view)
		}
	}
	fit("plain list")

	// And it fills the terminal: reserving a line for a hint that repeats the
	// help line just under it costs a row of list on every run. Scrolled deep
	// with more rows below, every optional line is drawn, so the frame is
	// exactly m.height - anything less is chrome that could be list.
	if lines := strings.Split(strings.TrimRight(m.View(), "\n"), "\n"); len(lines) != m.height {
		t.Errorf("view is %d lines for a %d-line terminal, want it to fill it:\n%s",
			len(lines), m.height, m.View())
	}

	m.menu, m.menuIdx = true, 0
	fit("sub-menu open")

	m.menu = false
	m.details = true
	fit("details open")

	// Both panels open: only one draws, and the frame still fits. Two panels
	// each sized against half the body would leave the list nothing.
	m.menu, m.details = true, true
	fit("both panels open")
}

// The header says where the client is pointed and whether it is live/restored.
func TestUIHeaderCountsEntities(t *testing.T) {
	m := uiFixture() // 5 entities, one of them read-only
	m.setStatus("", statusOK)
	m.defaultFolded = true

	view := stripANSI(m.View())
	if strings.Contains(view, "4/5") {
		t.Errorf("header should not carry the entity counts:\n%s", view)
	}
	// It counts what the list can reach, so the frame is still 4/5 with every
	// group closed - counting visible rows would say 0, on the one frame that
	// most needs the number.
	if strings.Contains(view, "0/5") {
		t.Errorf("counted visible rows instead of reachable entities:\n%s", view)
	}
}

// The view has to fit the terminal's width as well as its height. A rule
// hardcoded at 72 wrapped on a 60-column terminal, and a wrapped rule is a
// lost line: the frame grows past the height and bubbletea keeps the last
// height lines, so the title and filter scroll off the top.
func TestUIViewFitsTerminalWidth(t *testing.T) {
	m := uiFixture()
	for i := 0; i < 40; i++ {
		m.states = append(m.states, State{
			EntityID:   fmt.Sprintf("light.bulk_%02d", i),
			State:      "off",
			Attributes: map[string]any{"friendly_name": fmt.Sprintf("Bulk %02d", i)},
		})
	}
	m.setStatus("light.kitchen_light → on", statusOK)
	m.cursor = 20

	for _, width := range []int{80, 60, 40} {
		m.width = width
		m.height = 24
		view := m.View()
		for i, line := range strings.Split(strings.TrimRight(view, "\n"), "\n") {
			if n := len([]rune(stripANSI(line))); n > width {
				t.Errorf("width %d: line %d is %d columns wide:\n%s", width, i, n, line)
			}
		}
	}
}

// stripANSI removes SGR escape sequences so a rendered line can be measured.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// An entity with no friendly_name has no name: the name cell stays empty and
// the id moves to the id column, so every id in the list sits at one indent and
// an id is never passed off as a name.
func TestUINamelessEntityRendersIDNotName(t *testing.T) {
	m := uiFixture()
	m.states = append(m.states, State{EntityID: "light.workspace_light"}) // no attributes
	m.resetCursor()
	m.cursor = len(m.rows()) - 1
	t.Log("\n" + m.View())

	// "   " + pointer(2) + " " + state(stateWidth) + " " + name(nameWidth) + "  "
	const idColumn = 3 + 2 + 1 + stateWidth + 1 + nameWidth + 2
	for _, want := range []string{"light.workspace_light", "light.garage_light"} {
		var at int
		for i, r := range m.rows() {
			if !r.isGroup && r.entity.EntityID == want {
				at = strings.Index(m.line(r, i == m.cursor), want)
			}
		}
		if at != idColumn {
			t.Errorf("%s starts at column %d, want %d - ids must all line up:\n%s",
				want, at, idColumn, m.View())
		}
	}
}

func TestUIIsActive(t *testing.T) {
	for _, s := range []string{"on", "open", "unlocked", "playing", "home"} {
		if !isActive(State{State: s}) {
			t.Errorf("%q should count as active", s)
		}
	}
	for _, s := range []string{"off", "unknown", "unavailable", "above_horizon", ""} {
		if isActive(State{State: s}) {
			t.Errorf("%q should not count as active", s)
		}
	}
}

// Without service data we must show everything rather than nothing.
func TestUIRegistryUnavailable(t *testing.T) {
	m := uiFixture()
	mm, _ := m.Update(servicesMsg{err: errNotConfigured})
	got := mm.(*uiModel)
	if !got.showAll {
		t.Error("a failed service fetch should fall back to showing everything")
	}
	if !strings.Contains(got.View(), "sun.sun") {
		t.Error("fallback should reveal read-only entities")
	}
	if !strings.Contains(got.status, "could not read services") {
		t.Errorf("status should explain why, got %q", got.status)
	}
}

// A server that advertises services but no targets at all degrades the same
// way rather than showing an empty screen.
func TestUIRegistryNoTargets(t *testing.T) {
	m := uiFixture()
	mm, _ := m.Update(servicesMsg{services: []Service{{Domain: "light", Name: "turn_on"}}})
	got := mm.(*uiModel)
	if !got.showAll {
		t.Error("no target data should fall back to showing everything")
	}
	if !strings.Contains(got.status, "no service targets") {
		t.Errorf("status should explain, got %q", got.status)
	}
}

func pressRune(m *uiModel, r rune) (*uiModel, tea.Cmd) {
	var mm tea.Model = m
	var cmd tea.Cmd
	mm, cmd = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	return mm.(*uiModel), cmd
}

func TestUIViewCycle(t *testing.T) {
	m := uiFixture()
	if got := m.currentView(); got != "groups" {
		t.Fatalf("expected initial view to be groups, got %q", got)
	}

	// 'v' key outside search mode cycles the view
	m, _ = pressRune(m, 'v')
	if got := m.currentView(); got != "areas" {
		t.Errorf("expected view to cycle to areas, got %q", got)
	}
	if !strings.Contains(m.status, "view: areas") {
		t.Errorf("status should report view change, got %q", m.status)
	}

	m, _ = pressRune(m, 'v')
	if got := m.currentView(); got != "states" {
		t.Errorf("expected view to cycle to states, got %q", got)
	}

	m, _ = pressRune(m, 'v')
	if got := m.currentView(); got != "flat" {
		t.Errorf("expected view to cycle to flat, got %q", got)
	}

	m, _ = pressRune(m, 'v')
	if got := m.currentView(); got != "groups" {
		t.Errorf("expected view to cycle back to groups, got %q", got)
	}

	// Ctrl+V also cycles the view
	m, _ = press(m, tea.KeyCtrlV)
	if got := m.currentView(); got != "areas" {
		t.Errorf("expected Ctrl+V to cycle to areas, got %q", got)
	}
}

func TestUIViewAreas(t *testing.T) {
	m := uiFixture()
	m.states = []State{
		{EntityID: "light.spot_1", State: "on", Attributes: map[string]any{"area_name": "Kitchen", "friendly_name": "Spot 1"}},
		{EntityID: "light.spot_2", State: "off", Attributes: map[string]any{"area_id": "kitchen", "friendly_name": "Spot 2"}},
		{EntityID: "light.garage", State: "off", Attributes: map[string]any{"friendly_name": "Garage Light"}},
		{EntityID: "switch.coffee", State: "off", Attributes: map[string]any{"friendly_name": "Coffee Maker"}},
	}
	m.areas = map[string]string{
		"light.garage": "Garage",
		"kitchen":      "Kitchen", // maps area_id -> area_name
	}

	m.setView("areas")
	groups := groupRows(m)
	// Groups should be Kitchen, Garage, unknown (ordered by size, then name)
	if len(groups) != 3 {
		t.Fatalf("expected 3 area groups, got %v", groups)
	}
	if groups[0] != "Kitchen" {
		t.Errorf("expected Kitchen to be largest group, got %v", groups)
	}

	rows := m.rows()
	var inKitchen []string
	for _, r := range rows {
		if !r.isGroup && r.domain == "Kitchen" {
			inKitchen = append(inKitchen, r.entity.EntityID)
		}
	}
	if len(inKitchen) != 2 || inKitchen[0] != "light.spot_1" || inKitchen[1] != "light.spot_2" {
		t.Errorf("expected spot_1 and spot_2 in Kitchen, got %v", inKitchen)
	}

	// Verify header does not contain view mode (kept clean)
	view := m.View()
	if strings.Contains(view, "by areas") {
		t.Errorf("header should not show view mode:\n%s", view)
	}
}

func TestUIViewStates(t *testing.T) {
	m := uiFixture()
	m.setView("states")

	groups := groupRows(m)
	if len(groups) != 2 {
		t.Fatalf("expected 2 state groups (off, on), got %v", groups)
	}
	if groups[0] != "off" || groups[1] != "on" {
		t.Errorf("expected off (3 items) then on (1 item), got %v", groups)
	}

	var inOn []string
	for _, r := range m.rows() {
		if !r.isGroup && r.domain == "on" {
			inOn = append(inOn, r.entity.EntityID)
		}
	}
	if len(inOn) != 1 || inOn[0] != "light.kitchen_light" {
		t.Errorf("expected light.kitchen_light in 'on' group, got %v", inOn)
	}
}

func TestUIViewFlat(t *testing.T) {
	m := uiFixture()
	m.setView("flat")

	groups := groupRows(m)
	if len(groups) != 1 || groups[0] != "all" {
		t.Fatalf("expected 1 group named 'all', got %v", groups)
	}

	ents := entityRows(m)
	if len(ents) != 4 {
		t.Fatalf("expected 4 actionable entities in flat view, got %d: %v", len(ents), ents)
	}
}

func TestUIViewActionsInDifferentViews(t *testing.T) {
	m := uiFixture()
	m.states = []State{
		{EntityID: "light.kitchen_light", State: "on", Attributes: map[string]any{"area_name": "Kitchen", "friendly_name": "Kitchen Light"}},
	}
	m.setView("areas")
	m.resetCursor()

	// Highlighting light.kitchen_light in areas view
	acts := m.actionsFor(m.cursor)
	if len(acts) == 0 {
		t.Fatal("entity should have actions from its domain even in areas view")
	}
	var foundToggle, foundTurnOn, foundFav bool
	for _, a := range acts {
		switch {
		case a.service == "toggle":
			foundToggle = true
		case a.service == "turn_on":
			foundTurnOn = true
		case a.local:
			foundFav = true
		}
	}
	if !foundToggle || !foundTurnOn || !foundFav {
		t.Errorf("expected light actions (toggle, turn_on, favorite), got %v", acts)
	}

	// In states view
	m.setView("states")
	m.resetCursor()
	acts = m.actionsFor(m.cursor)
	if len(acts) == 0 {
		t.Fatal("entity should have actions from its domain in states view")
	}
}

func TestUIViewMaintainsCursor(t *testing.T) {
	m := uiFixture()
	// Place cursor on switch.coffee
	for i, r := range m.rows() {
		if !r.isGroup && r.entity.EntityID == "switch.coffee" {
			m.cursor = i
			break
		}
	}
	if m.selected().EntityID != "switch.coffee" {
		t.Fatalf("precondition failed: cursor should be on switch.coffee")
	}

	// Toggle view - switch.coffee should still be selected
	m.cycleViewAndSelect()
	if got := m.selected().EntityID; got != "switch.coffee" {
		t.Errorf("cursor should stay on switch.coffee across view toggle, got %q", got)
	}
}

func TestUIViewFilterInAreasAndStates(t *testing.T) {
	m := uiFixture()
	m.states = []State{
		{EntityID: "light.spot_1", State: "on", Attributes: map[string]any{"area_name": "Basement", "friendly_name": "Spot 1"}},
		{EntityID: "light.spot_2", State: "off", Attributes: map[string]any{"area_name": "Kitchen", "friendly_name": "Spot 2"}},
	}

	// In areas view, filtering for "basement" finds Spot 1
	m.setView("areas")
	m = typeRunes(m, "basement")
	ents := entityRows(m)
	if len(ents) != 1 || ents[0] != "light.spot_1" {
		t.Errorf("filtering by area name failed, got %v", ents)
	}

	// In states view, filtering for "off" finds Spot 2
	m2 := uiFixture()
	m2.states = []State{
		{EntityID: "light.spot_1", State: "on", Attributes: map[string]any{"friendly_name": "Spot 1"}},
		{EntityID: "light.spot_2", State: "off", Attributes: map[string]any{"friendly_name": "Spot 2"}},
	}
	m2.setView("states")
	m2 = typeRunes(m2, "off")
	ents2 := entityRows(m2)
	if len(ents2) != 1 || ents2[0] != "light.spot_2" {
		t.Errorf("filtering by state failed, got %v", ents2)
	}
}

func TestUIAreasMsgUpdatesModel(t *testing.T) {
	m := uiFixture()
	m.setView("areas")

	// Initially all entities without attribute areas are in "unknown"
	if len(groupRows(m)) != 1 || groupRows(m)[0] != "unknown" {
		t.Fatalf("expected all in unknown before areas arrive, got %v", groupRows(m))
	}

	// areasMsg arrives from background fetch
	mm, _ := m.Update(areasMsg{
		areas: map[string]string{
			"light.kitchen_light":   "Kitchen",
			"light.kitchen_light_2": "Kitchen",
			"light.garage_light":    "Garage",
			"switch.coffee":         "Kitchen",
		},
	})
	got := mm.(*uiModel)
	groups := groupRows(got)
	// Groups should now be Kitchen and Garage (unknown has 0 actionable entities so drops out)
	if len(groups) != 2 || groups[0] != "Kitchen" || groups[1] != "Garage" {
		t.Errorf("expected Kitchen and Garage groups after areasMsg, got %v", groups)
	}
}

// A failed areas fetch is silent by nature - the areas view then shows every
// entity under "unknown", which looks like a house with no areas. Say so.
func TestUIAreasFailureIsReported(t *testing.T) {
	m := uiFixture()
	m.setView("areas")
	mm, cmd := m.Update(areasMsg{err: errNotConfigured})
	got := mm.(*uiModel)
	if cmd != nil {
		t.Error("a failed areas fetch should not schedule anything")
	}
	if got.kind != "err" || !strings.Contains(got.status, "could not read areas") {
		t.Errorf("status = %q (kind %q)", got.status, got.kind)
	}
	if !strings.Contains(got.View(), "unknown") {
		t.Error("the list should still render, grouped under unknown")
	}
}

// A favorite is a second copy, not a move: it goes to the top group and stays
// where it was. In every view - the groups view used to drop it from its domain
// outright, which made pinning an entity look like deleting it from the house.
func TestUIFavoritesStayInTheirGroup(t *testing.T) {
	m := uiFixture()
	m.cfg.Favorites = []string{"light.kitchen_light"}
	m.areas = map[string]string{
		"light.kitchen_light":   "Kitchen",
		"light.kitchen_light_2": "Kitchen",
		"light.garage_light":    "Garage",
		"switch.coffee":         "Kitchen",
	}
	// where the entity is expected to also appear, per view
	stay := map[string]string{
		"groups": "light",
		"areas":  "Kitchen",
		"states": "on",
		"flat":   "all",
	}
	for view, group := range stay {
		m.setView(view)
		groups := groupRows(m)
		if len(groups) == 0 || groups[0] != favoritesGroup {
			t.Fatalf("%s: favorites should be the top group, got %v", view, groups)
		}
		found := false
		for _, r := range m.rows() {
			if !r.isGroup && r.domain == group && r.entity.EntityID == "light.kitchen_light" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: light.kitchen_light should also be in %q:\n%s", view, group, m.View())
		}
	}
	// The header says what the group is: it is the one group that is not a
	// domain, an area or a state, so it is marked.
	m.setView("groups")
	header := stripANSI(m.line(m.rows()[0], true))
	if !strings.Contains(header, "\u2605 favorites") {
		t.Errorf("favorites header should be starred: %q", header)
	}
	// And the group count is its own, not the whole list.
	if got := groupRows(m); len(got) < 2 || got[1] != "light" {
		t.Errorf("the domain group should follow favorites, got %v", got)
	}
}

// Pinning adds a group and a row above everything else, and unpinning takes one
// away, so every index below the cursor shifts. The cursor has to end up back on
// the row it was on — same entity *in the same group*, because a favorite now
// exists twice and the other copy sits at the top of the list.
func TestUICursorStaysPutWhenReshaping(t *testing.T) {
	type at struct {
		group, entity string
	}
	where := func(t *testing.T, m *uiModel) at {
		t.Helper()
		r, ok := m.rowAt(m.cursor)
		if !ok {
			t.Fatalf("no row at cursor %d", m.cursor)
		}
		if r.isGroup {
			return at{group: r.domain}
		}
		return at{group: r.domain, entity: r.entity.EntityID}
	}
	on := func(t *testing.T, m *uiModel, want at) {
		t.Helper()
		if got := where(t, m); got != want {
			t.Fatalf("cursor at %+v, want %+v", got, want)
		}
	}

	t.Run("pinning keeps the row under the cursor", func(t *testing.T) {
		m := uiFixture()
		t.Setenv("HAS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
		m.resetCursor()
		for i, r := range m.rows() {
			if !r.isGroup && r.entity.EntityID == "switch.coffee" {
				m.cursor = i
			}
		}
		before := m.cursor
		on(t, m, at{"switch", "switch.coffee"})

		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
		got := mm.(*uiModel)
		// The other copy of the entity is now the top row of the list, so
		// matching on the entity alone would put the cursor up there.
		on(t, got, at{"switch", "switch.coffee"})
		if got.cursor == before {
			t.Error("the favorites group really does add rows; the index should have moved")
		}
		if r, _ := got.rowAt(0); r.domain != favoritesGroup {
			t.Errorf("precondition: the second copy should be at the top, got %+v", r)
		}
	})

	t.Run("unpinning from the favorites copy lands on the entity", func(t *testing.T) {
		m := uiFixture()
		t.Setenv("HAS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
		m.cfg.Favorites = []string{"switch.coffee"}
		m.resetCursor()
		on(t, m, at{favoritesGroup, "switch.coffee"})

		// The row under the cursor is the one unpinning deletes, so the exact row
		// cannot be followed: the same entity in its own group is the best answer.
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
		got := mm.(*uiModel)
		if got.cfg.isFavorite("switch.coffee") {
			t.Fatal("precondition: should have unpinned")
		}
		on(t, got, at{"switch", "switch.coffee"})
	})

	t.Run("reordering keeps the group under the cursor", func(t *testing.T) {
		m := uiFixture()
		t.Setenv("HAS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
		m.resetCursor()
		// On a group header, so the header itself has to stay put.
		m.cursor = 0
		was, _ := m.rowAt(0)
		if !was.isGroup {
			t.Fatalf("precondition: row 0 should be a group, got %+v", was)
		}
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlN, Alt: true})
		got := mm.(*uiModel)
		if r, _ := got.rowAt(got.cursor); !r.isGroup || r.domain != was.domain {
			t.Errorf("cursor left %q for %+v", was.domain, r)
		}
	})
}

func TestUIHelpToggle(t *testing.T) {
	m := uiFixture()
	if m.help {
		t.Fatal("help panel should be closed initially")
	}
	// Pressing '?' toggles help open
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m = mm.(*uiModel)
	if !m.help {
		t.Errorf("expected help to be true after '?'")
	}
	v := m.View()
	if !strings.Contains(v, "keyboard shortcuts:") {
		t.Errorf("expected View to contain shortcuts panel when help is active:\n%s", v)
	}
	if !strings.Contains(stripANSI(v), "┌") {
		t.Errorf("expected the shortcuts to draw as a boxed popup:\n%s", v)
	}
	// esc closes help
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(*uiModel)
	if m.help {
		t.Errorf("expected help to be closed after esc")
	}
}

// Help is a popup, not a panel: it covers part of the list and the list keeps
// every row it had. Spliced in under the cursor row it read as a help screen
// the highlighted group had swallowed, and the rows below it jumped.
func TestUIHelpIsOverlay(t *testing.T) {
	m := uiFolded()
	m.height, m.width = 24, 80
	plain := strings.Split(strings.TrimRight(m.View(), "\n"), "\n")

	m.help = true
	lines := strings.Split(strings.TrimRight(m.View(), "\n"), "\n")
	if len(lines) < len(plain) {
		t.Errorf("popup shrank the view: %d lines, was %d:\n%s", len(lines), len(plain), m.View())
	}
	// Every key fits on a default terminal. A box trimmed to "… n more" hides
	// the entries below the cut, and one centred in a short view loses its own
	// bottom half - which is what happened with every group closed, the frame
	// the first run of `?` is pressed on.
	if !strings.Contains(stripANSI(m.View()), "quit") {
		t.Errorf("the popup is truncated:\n%s", stripANSI(m.View()))
	}
	if strings.Contains(stripANSI(m.View()), "more") {
		t.Errorf("the popup does not fit:\n%s", stripANSI(m.View()))
	}
	if len(lines) > m.height {
		t.Errorf("view is %d lines in a %d-line terminal", len(lines), m.height)
	}
	// It covers lines, it does not reflow them: the only rows that differ from
	// the plain view are the ones inside the box. Padding to the terminal
	// height adds blank lines below the list, which is not a change to it.
	covered := 0
	for i, l := range lines {
		if i < len(plain) && l != plain[i] {
			covered++
		}
	}
	if covered == 0 {
		t.Errorf("popup drew nothing over the list:\n%s", m.View())
	}
	if box := len(m.helpPopup()) + 2; covered > box {
		t.Errorf("popup changed %d lines, more than its own %d:\n%s", covered, box, m.View())
	}
	// Nothing leaks past the terminal width.
	for i, l := range lines {
		if n := len([]rune(stripANSI(l))); n > m.width {
			t.Errorf("line %d is %d columns wide:\n%s", i, n, stripANSI(l))
		}
	}
}

func TestUIViewPersistsConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HAS_CONFIG", dir)

	m := uiFixture()
	m.setView("groups")
	m.cycleViewAndSelect() // switches to "areas"

	loaded, err := loadUIConfig()
	if err != nil {
		t.Fatalf("loadUIConfig failed: %v", err)
	}
	if loaded.View != "areas" {
		t.Errorf("expected persisted View 'areas', got %q", loaded.View)
	}
}
