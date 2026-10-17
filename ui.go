package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/coder/websocket"
)

// pollInterval is how often the highlighted entity is re-read.
const pollInterval = 3 * time.Second

// hoverDelay debounces the re-read while the cursor is moving.
const hoverDelay = 120 * time.Millisecond

// Column widths for the entity rows. pad() counts runes, so a state like a
// full ISO timestamp is cut to exactly stateWidth rather than shoving the rest
// of the row sideways.
const (
	stateWidth = 13
	nameWidth  = 30
)

// entityGutter is the fixed cost of an entity row in columns, before the name
// and the id: 3 of indent (one for the frame, two so a row reads as belonging
// to the group header above it), the 2-wide pointer, a space, the state, a
// space, and the 2-space gap in front of the id.
const entityGutter = 3 + 2 + 1 + stateWidth + 1 + 2

// defaultHeight is used before the first WindowSizeMsg.
const defaultHeight = 24

// defaultWidth is the same. Without a WindowSizeMsg the layout falls back to
// these rather than collapsing: a model built by a test has no terminal to
// measure, and tests that care set both fields themselves.
const defaultWidth = 80

// row is one line of the list: either a group header or one entity inside it.
type row struct {
	isGroup bool
	domain  string
	entity  State
	// total and on are the whole-domain summary for a header row, computed
	// once in rows() rather than per drawn line. A non-group row ignores them.
	total int
	on    int
}

// fetchMsg carries a refreshed entity list, or the failure that prevented it.
type fetchMsg struct {
	states []State
	err    error
}

// servicesMsg carries the action registry, built once at start.
type servicesMsg struct {
	services []Service
	err      error
}

// areasMsg carries the entity -> area mapping, built once at start.
type areasMsg struct {
	areas map[string]string
	err   error
}

// oneMsg is a single entity's fresh state, fetched for the highlighted row
// only. Polling all 387 entities every few seconds is wasteful; the row you
// are looking at is the one that has to be right.
type oneMsg struct {
	state State
	gen   int
	err   error
}

// toggleMsg is the outcome of a service call made from the UI. state is the
// server's own new state for that entity, not just its state string: patching a
// bare {id, state} replaces the whole entry and takes friendly_name with it,
// which blanks the name on the row you just acted on.
type toggleMsg struct {
	entity string
	state  *State // nil when the call changed nothing, or failed
	err    error
}

// statusKind colours the status line. Two values, so a bool would read worse at
// the twenty-odd call sites than the name does - and a typo in a string literal
// would colour an error as a success silently, which a named type cannot.
type statusKind string

const (
	statusOK  statusKind = "ok"
	statusErr statusKind = "err"
)

type uiModel struct {
	client *Client
	states []State
	reg    registry
	filter string
	cursor int
	status string
	kind   statusKind
	// folded records explicit tab presses. A domain with no entry follows
	// defaultFolded, so starting collapsed does not mean seeding 16 entries.
	folded        map[string]bool
	defaultFolded bool
	showAll       bool // ^A: include domains with no actions
	// view selects how entities are grouped in the list: "groups" (the
	// default) groups by domain, "areas" by the entity's area, "states" by
	// state and "flat" by nothing at all. It is a view toggle, not a filter,
	// so it never changes which entities match. Always a value parseView
	// returned, never a raw --view string.
	view string
	// areas maps entity_id to friendly area name, from Client.entityAreas.
	// Empty until areasMsg lands, and empty for entities that have no area:
	// they group under "unknown".
	areas   map[string]string
	cfg     uiConfig
	menu    bool // the action sub-menu is open for the highlighted row
	details bool // ^D: show every attribute of the highlighted entity
	help    bool // ?: show keyboard shortcuts help panel
	menuIdx int
	height  int
	ready   bool

	// searching is true only after '/' has been pressed: everything typed
	// while it is true becomes filter text. While it is false, typing does
	// nothing, so a houseful of mappings never starts filtering by accident.
	searching bool
	// gen invalidates a pending hover fetch when the cursor moves again.
	gen int

	// Live updates come from the WebSocket when it works and from a re-read of
	// the highlighted row when it does not. wantWS is off only when the user
	// asked for no auto-refresh at all (--poll 0).
	conn     *websocket.Conn // nil unless we are subscribed right now
	interval time.Duration   // poll interval for the fallback path, 0 disables live updates
	wantWS   bool
	polling  bool // re-reading the highlighted row, the fallback path
	tries    int  // consecutive connect attempts, for the backoff

	width int // terminal width, 0 before the first WindowSizeMsg
}

func cmdUI(args []string) error {
	fs := newFlagSet("ui")
	interval := fs.Duration("poll", pollInterval, "poll interval when the websocket is unavailable, 0 to disable live updates")
	expand := fs.Bool("expand", false, "start with every group expanded")
	mouse := fs.Bool("mouse", false, "click to select, wheel to scroll (disables terminal text selection)")
	view := fs.String("view", "", "grouping: groups (by domain), areas, states, or flat; remembered between runs")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	// Validated here rather than in setView, so a typo on the command line is
	// an error instead of a silent fall back to the default grouping.
	chosen, err := parseView(*view)
	if err != nil {
		return err
	}
	c, err := configured()
	if err != nil {
		return err
	}
	cfg, err := loadUIConfig()
	if err != nil {
		return err
	}
	opts := []tea.ProgramOption{tea.WithAltScreen()}
	m := &uiModel{
		client:   c,
		height:   defaultHeight,
		width:    defaultWidth,
		folded:   map[string]bool{},
		cfg:      cfg,
		interval: *interval,
		wantWS:   *interval > 0,
		polling:  *interval > 0,
		// Every run starts with the list closed. What ui.json restores is
		// preferences - the group order, favorites, the grouping view - and
		// never a snapshot: which groups you happened to leave open yesterday
		// is not a preference, and a saved one made the list start at 387 rows.
		defaultFolded: !*expand,
	}
	// The flag wins; an empty one means "use whatever ui.json remembers",
	// normalised so an alias in the file still resolves.
	if *view == "" && cfg.View != "" {
		chosen, _ = parseView(cfg.View)
	}
	m.setView(string(chosen))
	// Clicking a row beats typing a filter, but enabling the mouse takes it away
	// from terminal text selection, so it stays opt-in.
	if *mouse {
		opts = append(opts, tea.WithMouseCellMotion())
	}
	if _, err := tea.NewProgram(m, opts...).Run(); err != nil {
		return err
	}
	return nil
}

func (m *uiModel) Init() tea.Cmd {
	cmds := []tea.Cmd{fetchCmd(m.client), servicesCmd(m.client), areasCmd(m.client)}
	if m.wantWS {
		// The handshake gets the request timeout, not the poll interval: three
		// seconds is fine to re-read one row and not fine to authenticate.
		cmds = append(cmds, wsCmd(m.client, *flagTimeout, wsStateChanged))
	}
	cmds = append(cmds, m.scheduleNext())
	return tea.Batch(cmds...)
}

// scheduleNext queues the next hover re-read. It only runs while the WebSocket
// is not subscribed: with live events there is nothing to ask for.
func (m *uiModel) scheduleNext() tea.Cmd {
	if !m.polling || m.interval <= 0 {
		return nil
	}
	return tea.Tick(m.interval, func(time.Time) tea.Msg { return pollMsg{} })
}

// schedulePing keeps proving the socket is alive.
func (m *uiModel) schedulePing() tea.Cmd {
	if m.conn == nil {
		return nil
	}
	return tea.Tick(wsPingInterval, func(time.Time) tea.Msg { return wsPingMsg{} })
}

type pollMsg struct{}

// wsPingMsg fires the liveness probe.
type wsPingMsg struct{}

// wsRetryMsg fires a reconnect attempt after the backoff.
type wsRetryMsg struct{}

// closeWS drops the socket. Every path out goes through here, so a reconnect
// never leaves the previous connection open.
func (m *uiModel) closeWS() {
	if m.conn != nil {
		m.conn.CloseNow()
		m.conn = nil
	}
}

// backoff spaces out reconnect attempts so a server that is down is not asked
// once a second.
func (m *uiModel) backoff() time.Duration {
	return wsBackoff(m.tries)
}

func fetchCmd(c *Client) tea.Cmd {
	return func() tea.Msg {
		states, err := c.states()
		return fetchMsg{states, err}
	}
}

func servicesCmd(c *Client) tea.Cmd {
	return func() tea.Msg {
		svcs, err := c.services()
		return servicesMsg{services: svcs, err: err}
	}
}

func areasCmd(c *Client) tea.Cmd {
	return func() tea.Msg {
		areas, err := c.entityAreas()
		return areasMsg{areas: areas, err: err}
	}
}

func fetchOneCmd(c *Client, entity string, gen int) tea.Cmd {
	return func() tea.Msg {
		s, err := c.state(entity)
		return oneMsg{state: s, gen: gen, err: err}
	}
}

// hoverCmd debounces: moving down through 132 sensors must not fire 132
// requests, only the one you settle on.
func (m *uiModel) hoverCmd() tea.Cmd {
	e := m.selected().EntityID
	if e == "" {
		return nil
	}
	m.gen++
	gen := m.gen
	return tea.Tick(hoverDelay, func(time.Time) tea.Msg { return hoverMsg{gen: gen, entity: e} })
}

type hoverMsg struct {
	gen    int
	entity string
}

// actionCmd fires an action against one entity. The service call already comes
// back with that entity's new state, so there is no second request to make.
func actionCmd(c *Client, entity, domain, service string) tea.Cmd {
	return func() tea.Msg {
		changed, err := c.call(domain, service, map[string]any{"entity_id": []string{entity}})
		if err != nil {
			return toggleMsg{entity: entity, err: err}
		}
		// Home Assistant answers with every state the call changed, and a light
		// group is one of them: turning on a member can report the group first.
		// Take the row for the entity that was actually acted on.
		for _, s := range changed {
			if s.EntityID == entity {
				return toggleMsg{entity: entity, state: &s}
			}
		}
		return toggleMsg{entity: entity}
	}
}

func (m *uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
		m.width = msg.Width
		return m, nil

	case fetchMsg:
		if msg.err != nil {
			m.ready = true
			m.setStatus(msg.err.Error(), statusErr)
			return m, nil
		}
		first := !m.ready
		m.ready = true
		m.states = msg.states
		// Only on the first load: a refresh must never yank the cursor out
		// from under someone who is navigating.
		if first {
			m.resetCursor()
		}
		m.keepCursorInRange()
		return m, m.scheduleNext()

	case servicesMsg:
		if msg.err != nil {
			// Without the service list we cannot tell actionable from
			// read-only. Show everything rather than nothing.
			m.showAll = true
			m.setStatus("could not read services, showing every entity: "+msg.err.Error(), statusErr)
			return m, nil
		}
		m.reg = newRegistry(msg.services)
		if !m.reg.usable {
			m.showAll = true
			m.setStatus("server reported no service targets, showing every entity", statusErr)
			return m, nil
		}
		// A different action set means different rows; start over cleanly.
		m.resetCursor()
		m.keepCursorInRange()
		return m, m.hoverCmd()

	case areasMsg:
		// A failure here is silent by nature: the areas view then groups every
		// entity under "unknown" and looks like a house with no areas at all.
		// Say so, or nobody thinks to look.
		if msg.err != nil {
			m.setStatus("could not read areas, every entity shows as unknown: "+msg.err.Error(), statusErr)
			return m, nil
		}
		if len(msg.areas) == 0 {
			return m, nil
		}
		was, _ := m.rowAt(m.cursor)
		m.areas = msg.areas
		// Keep the cursor on the same entity: the groups around it are all
		// different now, so its index is not.
		m.follow(was)
		return m, nil

	case hoverMsg:
		if msg.gen != m.gen {
			return m, nil // superseded by a later cursor move
		}
		return m, fetchOneCmd(m.client, msg.entity, msg.gen)

	case oneMsg:
		if msg.gen != m.gen || msg.err != nil {
			return m, nil
		}
		m.patch(msg.state)
		return m, nil

	case pollMsg:
		return m, tea.Batch(m.hoverCmd(), m.scheduleNext())

	case wsConnMsg:
		// Two connect attempts can overlap if a ping and a retry both noticed the
		// socket die; the loser has to be closed, not orphaned.
		m.closeWS()
		// Subscribed. Stop re-reading the highlighted row: events now do it.
		m.conn, m.polling, m.tries = msg.conn, false, 0
		m.setStatus("live updates on", statusOK)
		// Events that happened while we were away were never delivered, so take
		// a fresh full read rather than trusting the list we already had.
		return m, tea.Batch(wsReadCmd(msg.conn), m.schedulePing(), fetchCmd(m.client))

	case wsStateMsg:
		if msg.conn != m.conn {
			return m, nil // from a socket we already replaced
		}
		if msg.state == nil {
			m.drop(msg.entityID)
		} else {
			m.patch(*msg.state)
		}
		return m, wsReadCmd(m.conn)

	case wsIdleMsg:
		if msg.conn != m.conn {
			return m, nil
		}
		return m, wsReadCmd(m.conn)

	case wsAliveMsg:
		if msg.conn != m.conn {
			return m, nil
		}
		return m, m.schedulePing()

	case wsPingMsg:
		if m.conn == nil {
			return m, nil
		}
		return m, wsPingCmd(m.conn)

	case wsFailedMsg:
		// No socket. The highlighted row still gets re-read, which is what the
		// program did before this existed. A few retries cover a server that is
		// still starting; past that, stop asking and leave the user on polling.
		m.polling = m.interval > 0
		m.tries++
		if m.wantWS && m.tries < wsConnectTries {
			m.setStatus("live updates unavailable ("+msg.err.Error()+"), retrying", statusErr)
			return m, tea.Batch(m.scheduleNext(), tea.Tick(m.backoff(),
				func(time.Time) tea.Msg { return wsRetryMsg{} }))
		}
		m.wantWS = false
		m.setStatus("live updates unavailable ("+msg.err.Error()+"), polling instead", statusErr)
		return m, nil

	case wsRejectedMsg:
		m.wantWS = false
		m.setStatus(msg.err.Error()+" — run `has login`", statusErr)
		return m, nil

	case wsGoneMsg:
		if msg.conn != m.conn {
			return m, nil // a socket we already replaced; the live one stands
		}
		m.closeWS()
		m.polling = m.interval > 0
		m.setStatus("live updates lost ("+msg.err.Error()+"), reconnecting", statusErr)
		if !m.wantWS {
			return m, m.scheduleNext()
		}
		m.tries++
		return m, tea.Batch(m.scheduleNext(), tea.Tick(m.backoff(),
			func(time.Time) tea.Msg { return wsRetryMsg{} }))

	case wsRetryMsg:
		return m, wsCmd(m.client, *flagTimeout, wsStateChanged)

	case toggleMsg:
		if msg.err != nil {
			m.setStatus(msg.err.Error(), statusErr)
			return m, nil
		}
		if msg.state == nil {
			m.setStatus(fmt.Sprintf("called on %s", msg.entity), statusOK)
			return m, nil
		}
		m.setStatus(fmt.Sprintf("%s → %s", msg.entity, msg.state.State), statusOK)
		m.patch(*msg.state)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)
	}
	return m, nil
}

// runeKeys are the printable keys that do something outside a search. Each
// entry returns its own command: `?` toggles an overlay and must not schedule a
// hover re-read, while the other two move the cursor and must.
//
// The table is consulted only when m.searching is false, which is what keeps
// every letter free as filter text: the guard lives in one place rather than
// being repeated per entry, so a key added here cannot start eating somebody's
// query.
var runeKeys = map[rune]func(*uiModel) tea.Cmd{
	'f': func(m *uiModel) tea.Cmd { m.toggleFavorite(); return m.afterMove() },
	'v': func(m *uiModel) tea.Cmd { m.cycleViewAndSelect(); return m.afterMove() },
	'V': func(m *uiModel) tea.Cmd { m.cycleViewAndSelect(); return m.afterMove() },
	// It lands on top of the details panel rather than beside it, so that one
	// goes away - and going back to it is one ^D away.
	'?': func(m *uiModel) tea.Cmd { m.help = !m.help; m.details = false; return nil },
}

// Actions deliberately avoid letters: the filter is typed with letters, so
// "kitchen" must not be read as the k binding. While the action sub-menu is
// open the digits 1-9 run a menu entry directly, which is the one place they
// are not filter text.
func (m *uiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// With the sub-menu open the menu owns the keyboard: anything menuKey does
	// not handle is dropped rather than falling through to the list or the
	// filter, so no keystroke can move the selection or edit the filter under
	// an open menu.
	if m.menu {
		if cmd, done := m.menuKey(msg); done {
			return m, cmd
		}
		return m, nil
	}
	if !m.searching && len(msg.Runes) == 1 {
		if fn, ok := runeKeys[msg.Runes[0]]; ok {
			return m, fn(m)
		}
	}
	switch msg.Type {
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyEsc:
		// Go back, one layer at a time, and never quit: sub-menu, help, details,
		// filter, then one more group folded. It only ever closes — a press that
		// could also unfold would make two presses of esc undo each other, and
		// the point of pressing it repeatedly is to end up with everything shut.
		// ctrl+c is the only way out.
		if m.menu {
			m.closeMenu()
			return m, nil
		}
		if m.help {
			m.help = false
			return m, nil
		}
		if m.details {
			m.details = false
			return m, nil
		}
		if m.filter != "" || m.searching {
			m.filter = ""
			m.searching = false
			m.resetCursor()
			return m, m.afterMove()
		}
		m.foldDown()
		return m, m.afterMove()
	case tea.KeyCtrlN, tea.KeyDown:
		// alt+arrows reorder, which is a persistent change to the saved layout
		// and should never be a thing you hit while scrolling.
		if msg.Alt {
			return m, m.nudge(1)
		}
		m.move(1)
		return m, m.afterMove()
	case tea.KeyCtrlP, tea.KeyUp:
		if msg.Alt {
			return m, m.nudge(-1)
		}
		m.move(-1)
		return m, m.afterMove()
	case tea.KeyShiftDown, tea.KeyCtrlF, tea.KeyPgDown:
		m.page(1)
		return m, m.afterMove()
	case tea.KeyShiftUp, tea.KeyCtrlB, tea.KeyPgUp:
		m.page(-1)
		return m, m.afterMove()
	case tea.KeyHome:
		m.firstEntity()
		return m, m.afterMove()
	case tea.KeyEnd:
		m.lastEntity()
		return m, m.afterMove()
	case tea.KeyLeft:
		// Fold, the way a tree does, and land on the header you just closed.
		// Without that the cursor stays on the row index and silently becomes
		// the next group's header, so the following → unfolded the wrong one.
		if r, ok := m.rowAt(m.cursor); ok {
			m.setFolded(r.domain, true)
			m.focusHeader(r.domain)
			return m, nil
		}
		return m, nil
	case tea.KeyRight:
		if r, ok := m.rowAt(m.cursor); ok {
			m.setFolded(r.domain, false)
			return m, nil
		}
		return m, nil
	case tea.KeySpace:
		// Space is printable, so it is filter text inside a search ("living
		// room") and the primary action outside one. The two cannot collide:
		// typing only happens in search mode.
		if m.searching {
			m.filter += " "
			m.resetCursor()
			return m, m.afterMove()
		}
		return m, m.runAction(0)
	case tea.KeyEnter:
		if m.searching {
			m.searching = false
			return m, nil
		}
		if r, ok := m.rowAt(m.cursor); ok && r.isGroup {
			m.setFolded(r.domain, m.expanded(r.domain))
			m.keepCursorInRange()
			return m, nil
		}
		return m, m.toggleMenu()
	case tea.KeyCtrlD:
		m.closeMenu()
		m.help = false
		m.details = !m.details
		return m, nil
	case tea.KeyCtrlV:
		// ^G, ^T and ^O used to cycle the view too. Four keys for one action,
		// three of them in neither the help popup nor the README, is the same
		// shotgun a dead alias list is: nobody remembers which one to press.
		m.cycleViewAndSelect()
		return m, m.afterMove()
	case tea.KeyCtrlA:
		m.showAll = !m.showAll
		m.resetCursor()
		m.keepCursorInRange()
		what := "actionable entities only"
		if m.showAll {
			what = "all entities, including read-only ones"
		}
		m.setStatus("showing "+what, statusOK)
		return m, m.afterMove()
	case tea.KeyCtrlU:
		m.filter = ""
		m.searching = false
		m.resetCursor()
		return m, m.afterMove()
	case tea.KeyCtrlR:
		m.setStatus("refreshing…", statusOK)
		return m, tea.Batch(fetchCmd(m.client), areasCmd(m.client), m.afterMove())
	case tea.KeyBackspace:
		m.filter = dropLastRune(m.filter)
		m.resetCursor()
		return m, nil
	}

	return m, m.searchKey(msg)
}

// searchKey is what a printable does outside the switches above: "/" opens the
// search, every printable inside one is filter text, and outside one it does
// nothing at all.
func (m *uiModel) searchKey(msg tea.KeyMsg) tea.Cmd {
	if len(msg.Runes) != 1 {
		return nil
	}
	r := msg.Runes[0]
	if !m.searching {
		if r != '/' {
			return nil
		}
		m.searching = true
		m.filter = ""
		m.resetCursor()
		return m.afterMove()
	}
	m.filter += string(r)
	m.resetCursor()
	return m.afterMove()
}

// ---------------------------------------------------------------- sub-menu

// toggleMenu opens the action list for the highlighted entity. The first
// action stays bound to space so the common case is still one keystroke.
func (m *uiModel) toggleMenu() tea.Cmd {
	if m.menu {
		m.closeMenu()
		return nil
	}
	acts := m.actionsFor(m.cursor)
	if len(acts) == 0 {
		return m.runAction(0) // reports why it cannot
	}
	m.help = false
	m.menu = true
	m.menuIdx = 0
	return nil
}

func (m *uiModel) closeMenu() {
	m.menu = false
	m.menuIdx = 0
}

// menuKey handles input while the sub-menu is open.
func (m *uiModel) menuKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.Type {
	case tea.KeyEsc:
		m.closeMenu()
		return nil, true
	case tea.KeyCtrlN, tea.KeyDown:
		m.menuMove(1)
		return nil, true
	case tea.KeyCtrlP, tea.KeyUp:
		m.menuMove(-1)
		return nil, true
	case tea.KeyEnter, tea.KeySpace:
		// Run the chosen action without closing the menu, so the choice
		// sticks while the action's result arrives.
		i := m.menuIdx
		return m.runAction(i), true
	case tea.KeyCtrlC:
		return tea.Quit, true
	case tea.KeyRunes:
		// Digits run the action directly: a media_player has ten actions, and
		// arrows alone would take ten presses to reach the last one. Digits
		// are free here because the filter is not being typed into.
		if len(msg.Runes) == 1 && msg.Runes[0] >= '1' && msg.Runes[0] <= '9' {
			i := int(msg.Runes[0] - '1')
			acts := m.actionsFor(m.cursor)
			if i < len(acts) {
				// Keep the menu open, like enter and space do now.
				return m.runAction(i), true
			}
		}
		return nil, true
	}
	return nil, false
}

func (m *uiModel) menuMove(d int) {
	n := len(m.actionsFor(m.cursor))
	if n == 0 {
		return
	}
	m.menuIdx += d
	if m.menuIdx < 0 {
		m.menuIdx = 0
	}
	if m.menuIdx >= n {
		m.menuIdx = n - 1
	}
}

// viewMode is how entities are grouped in the list. One field holds it
// (uiModel.view) and the constants keep the names in one place: currentView()
// used to re-parse the string on every rows() call, which is once per entity
// per frame.
type viewMode string

const (
	viewGroups viewMode = "groups"
	viewAreas  viewMode = "areas"
	viewStates viewMode = "states"
	viewFlat   viewMode = "flat"
)

// parseView maps a --view value, or a saved ui.json entry, onto one of the
// four groupings. The aliases are what people actually type, so they are
// accepted; anything else is an error rather than a quiet default.
func parseView(s string) (viewMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "groups", "group", "domain", "domains", "default":
		return viewGroups, nil
	case "areas", "area":
		return viewAreas, nil
	case "states", "state", "status":
		return viewStates, nil
	case "flat", "list", "none", "all", "ungrouped":
		return viewFlat, nil
	}
	return "", fmt.Errorf("unknown view %q: choose groups, areas, states, or flat", s)
}

// ---------------------------------------------------------------- layout

// nudge moves whatever the cursor is on one place: a group through the order of
// the domains, or a favorite through the favorites. A row that is neither says
// why rather than guessing.
func (m *uiModel) nudge(delta int) tea.Cmd {
	r, ok := m.rowAt(m.cursor)
	if !ok {
		return nil
	}
	if !r.isGroup {
		return m.nudgeFavorite(r, delta)
	}
	return m.nudgeGroup(r, delta)
}

// nudgeFavorite moves a favorite up or down the favorites group. Favorites are
// the one place a single entity has an order of its own; anywhere else rows are
// sorted by id, so an entity outside favorites has nothing to move within and
// its keys move its domain group instead.
func (m *uiModel) nudgeFavorite(r row, delta int) tea.Cmd {
	id := r.entity.EntityID
	if r.domain != favoritesGroup || !m.cfg.isFavorite(id) {
		m.setStatus("only favorites have an order of their own · f pins one", statusOK)
		return nil
	}
	order := slices.Clone(m.cfg.Favorites)
	i := slices.Index(order, id)
	j := i + delta
	if j < 0 || j >= len(order) {
		m.setStatus(fmt.Sprintf("%s is already %s in favorites", friendlyName(&r.entity), endWord(delta)), statusOK)
		return nil
	}
	order[i], order[j] = order[j], order[i]

	next := m.cfg
	next.Favorites = order
	m.cfg = next
	if err := saveUIConfig(m.cfg); err != nil {
		m.setStatus("could not save ui.json: "+err.Error(), statusErr)
		return nil
	}
	m.setStatus(fmt.Sprintf("moved %s %s in favorites", friendlyName(&r.entity), endWord(delta)), statusOK)
	// The favorite moved one place; the cursor stays on it.
	m.follow(r)
	return nil
}

// nudgeGroup moves the highlighted group one place through the order the list is
// showing, then saves that order under the current view: domains for the groups
// view, areas for the areas view, states for the states view. One order each,
// because the names live in different spaces — an area order saved as a domain
// order would bury the domain one.
//
// It used to swap inside the saved pin list, which is a different thing: with
// nothing pinned - a fresh install, or anyone who has never pressed alt+up -
// every domain is unpinned, so there was nothing to swap inside, "move down"
// could never do anything, and the group at the top of the screen reported
// itself "already bottom". One concept now: the order is the whole order, and
// a move is a swap with the neighbour you can see.
func (m *uiModel) nudgeGroup(r row, delta int) tea.Cmd {
	// The swap is between the groups on screen, so every press moves something
	// you can see. The saved order also holds groups ^A is hiding, and trading
	// places with one of those reports a move that never happened on screen.
	shown := m.groupOrder(m.matched())
	i := slices.Index(shown, r.domain)
	j := i + delta
	if i < 0 || j < 0 || j >= len(shown) {
		m.setStatus(fmt.Sprintf("%s is already %s", r.domain, endWord(delta)), statusOK)
		return nil
	}
	shown[i], shown[j] = shown[j], shown[i]

	// Then whatever is not on screen, so a filtered or actionable-only list
	// cannot drop the groups it happens to be hiding.
	order := shown
	for _, d := range m.groupOrder(m.states) {
		if !slices.Contains(order, d) {
			order = append(order, d)
		}
	}

	m.cfg = m.cfg.withOrder(m.currentView(), order)
	if err := saveUIConfig(m.cfg); err != nil {
		m.setStatus("could not save ui.json: "+err.Error(), statusErr)
		return nil
	}
	m.setStatus(fmt.Sprintf("moved %s to %s", r.domain, endWord(delta)), statusOK)
	// The group moved, not the cursor: it used to reset to the first entity,
	// which threw you to the top of the list on every reorder.
	m.follow(r)
	return m.hoverCmd()
}

func endWord(delta int) string {
	if delta < 0 {
		return "top"
	}
	return "bottom"
}

// favoritesGroup is the name of the group favorites are gathered into. It is not
// a domain, an area or a state, which is why it is the one header marked with a
// star and the one that starts open.
const favoritesGroup = "favorites"

// actionsFor is the action list for the highlighted row, empty on a group.
func (m *uiModel) actionsFor(i int) []action {
	r, ok := m.rowAt(i)
	if !ok || r.isGroup {
		return nil
	}
	// An entity takes its actions from its real domain, regardless of how it
	// is grouped (domain, area, state, flat, or favorites).
	d := domain(r.entity.EntityID)
	acts := append([]action(nil), m.reg.actions(d)...)
	if len(acts) == 0 {
		// Nothing to act on, so no menu: "nothing to do with sun.sun" is the
		// honest answer, and space must not fall through to pinning an entity.
		// `f` still pins one, whatever it is.
		return nil
	}
	label := "favorite"
	if m.cfg.isFavorite(r.entity.EntityID) {
		label = "unfavorite"
	}
	acts = append(acts, action{label: label, order: 1000, local: true})
	return acts
}

// runAction fires the nth action of the highlighted entity.
func (m *uiModel) runAction(n int) tea.Cmd {
	r, ok := m.rowAt(m.cursor)
	if !ok {
		m.setStatus("nothing selected", statusErr)
		return nil
	}
	if r.isGroup {
		m.setStatus(m.foldHint(r.domain), statusOK)
		return nil
	}
	d := domain(r.entity.EntityID)
	// actionsFor, not the registry: the menu draws the favorite entry too, so
	// running entry n of the menu has to use the same list. Index 0 is still
	// the primary registry action, because the favorite is appended last.
	acts := m.actionsFor(m.cursor)
	if len(acts) == 0 {
		m.setStatus(fmt.Sprintf("nothing to do with %s", r.entity.EntityID), statusErr)
		return nil
	}
	if n >= len(acts) {
		m.setStatus(fmt.Sprintf("%s has %d actions, %d is not one", d, len(acts), n+1), statusErr)
		return nil
	}
	a := acts[n]
	if a.local {
		m.toggleFavorite()
		return nil
	}
	m.setStatus(fmt.Sprintf("%s %s…", a.label, r.entity.EntityID), statusOK)
	return actionCmd(m.client, r.entity.EntityID, d, a.service)
}

// toggleFavorite pins or unpins the highlighted entity at the top of the list.
// It writes ui.json synchronously so the next run — or right now — keeps it.
func (m *uiModel) toggleFavorite() {
	r, ok := m.rowAt(m.cursor)
	if !ok || r.isGroup {
		m.setStatus("nothing selected", statusErr)
		return
	}
	id := r.entity.EntityID
	m.cfg = m.cfg.toggleFavorite(id)
	if err := saveUIConfig(m.cfg); err != nil {
		m.setStatus("could not save ui.json: "+err.Error(), statusErr)
		return
	}
	word := "unfavorited"
	if m.cfg.isFavorite(id) {
		word = "favorited"
	}
	m.setStatus(fmt.Sprintf("%s %s", word, id), statusOK)
	// Pinning adds a group and a row above everything else, and unpinning takes
	// one away: the row under the cursor is not the row that was under it.
	m.follow(r)
}

// follow puts the cursor back on the row it was on, matched by identity rather
// than by index. Anything that adds or removes rows — pinning, reordering,
// switching grouping — shifts every index below it, so a cursor held by number
// lands on whatever slid into its place.
//
// The group is part of the identity, and that is the whole point of a favorite
// being a copy rather than a move: the same entity is in two groups at once, so
// matching the entity alone drags the cursor up to the favorites copy of the row
// you were standing on. When the exact row is gone — a new grouping calls the
// group something else, or the row was the copy that unpinning just deleted —
// the same entity is the next best thing.
func (m *uiModel) follow(was row) {
	rows := m.rows()
	if i, ok := indexRow(rows, was, true); ok {
		m.cursor = i
		return
	}
	if i, ok := indexRow(rows, was, false); ok {
		m.cursor = i
		return
	}
	m.keepCursorInRange()
}

// indexRow finds was among rows: the same entity, or the same group header.
// With sameGroup it insists on the group as well, which is what keeps the
// cursor off the favorites copy of the entity that was just pinned.
func indexRow(rows []row, was row, sameGroup bool) (int, bool) {
	for i, r := range rows {
		if was.isGroup != r.isGroup {
			continue
		}
		if was.isGroup {
			if r.domain == was.domain {
				return i, true
			}
			continue
		}
		if r.entity.EntityID == was.entity.EntityID && (!sameGroup || r.domain == was.domain) {
			return i, true
		}
	}
	return 0, false
}

// patch merges one entity's state in place, keeping group membership and
// ordering stable. A websocket state_changed event is exactly this shape.
//
// Attributes are carried over when the patch has none: three callers feed this
// (an event, a single-entity read, a service call) and a patch that arrives
// without them would otherwise blank friendly_name, which is the row's name.
func (m *uiModel) patch(s State) {
	if s.EntityID == "" {
		return
	}
	for i := range m.states {
		if m.states[i].EntityID != s.EntityID {
			continue
		}
		if len(s.Attributes) == 0 {
			s.Attributes = m.states[i].Attributes
		}
		m.states[i] = s
		return
	}
}

// drop removes an entity the server no longer has. A state_changed event with a
// null new_state is how Home Assistant says an entity went away.
func (m *uiModel) drop(entityID string) {
	out := m.states[:0]
	for _, s := range m.states {
		if s.EntityID != entityID {
			out = append(out, s)
		}
	}
	m.states = out
	m.keepCursorInRange()
}

func (m *uiModel) foldHint(d string) string {
	if m.expanded(d) {
		return fmt.Sprintf("← folds %s", d)
	}
	return fmt.Sprintf("→ expands %s", d)
}

// setFolded opens or closes one group. Fold state is per run: the list always
// starts closed, so there is nothing here to write to disk.
func (m *uiModel) setFolded(domain string, folded bool) {
	m.folded[domain] = folded
}

func (m *uiModel) setStatus(s string, kind statusKind) { m.status, m.kind = s, kind }

// ---------------------------------------------------------------- the tree

// matched is what the list shows. With no filter that is the entities you can
// act on, which for a large instance is a fraction of the total. Once you type,
// the search covers everything.
func (m *uiModel) matched() []State {
	if m.filter != "" {
		needle := strings.ToLower(m.filter)
		out, _ := resolve(m.states, []string{m.filter}, true) // a miss returns nil
		seen := map[string]bool{}
		for _, s := range out {
			seen[s.EntityID] = true
		}
		// The grouping's own value is searchable too: an area name or a state
		// is what is on screen, so it is what should be typeable. Groups and
		// flat have no such value and fall through with hay empty.
		for _, s := range m.states {
			if seen[s.EntityID] {
				continue
			}
			var hay string
			switch m.currentView() {
			case viewAreas:
				hay = m.areaName(s)
			case viewStates:
				hay = s.State
			}
			if hay != "" && strings.Contains(strings.ToLower(hay), needle) {
				out = append(out, s)
				seen[s.EntityID] = true
			}
		}
		return out
	}
	if m.showAll || !m.reg.usable {
		return m.states
	}
	out := make([]State, 0, len(m.states))
	for _, s := range m.states {
		if m.reg.controllable(domain(s.EntityID)) {
			out = append(out, s)
		}
	}
	return out
}

// isFolded is the saved preference for one group, ignoring any active filter.
func (m *uiModel) isFolded(d string) bool {
	if v, ok := m.folded[d]; ok {
		return v
	}
	if d == favoritesGroup || (d == "all" && m.currentView() == viewFlat) {
		return false // pinned group should be visible first; flat view starts open
	}
	return m.defaultFolded
}

// expanded reports whether a group's entities are visible. Filtering always
// reveals, otherwise searching would show nothing but headers.
func (m *uiModel) expanded(d string) bool {
	if m.filter != "" {
		return true
	}
	return !m.isFolded(d)
}

// rows is the flattened, visible list the cursor walks. Favorites go first, as
// their own group at the top - in the order ui.json gives them, which is the
// order alt+arrows moves them in - and they stay where they are as well. A
// favorite is a second copy, not a move: an entity you pinned to the top still
// belongs to its domain, its area and its state, and counting it in only one
// place made the other group look like it had lost something.
func (m *uiModel) rows() []row {
	matched := m.matched()
	byGroup := map[string][]State{}
	for _, s := range matched {
		key := m.groupKey(s)
		byGroup[key] = append(byGroup[key], s)
	}
	out := make([]row, 0, len(matched)+8)
	// Favorites are a group at the top when nothing is being filtered.
	if m.filter == "" {
		if favs := m.orderedFavorites(matched); len(favs) > 0 {
			g := row{isGroup: true, domain: favoritesGroup, total: len(favs)}
			for _, s := range favs {
				if isActive(s) {
					g.on++
				}
			}
			out = append(out, g)
			if m.expanded(favoritesGroup) {
				for _, s := range favs {
					out = append(out, row{domain: favoritesGroup, entity: s})
				}
			}
		}
	}
	for _, d := range m.groupOrder(matched) {
		ents := byGroup[d]
		if len(ents) == 0 {
			continue
		}
		sort.Slice(ents, func(i, j int) bool { return ents[i].EntityID < ents[j].EntityID })
		g := row{isGroup: true, domain: d, total: len(ents)}
		for _, s := range ents {
			if isActive(s) {
				g.on++
			}
		}
		out = append(out, g)
		if !m.expanded(d) {
			continue
		}
		for _, s := range ents {
			out = append(out, row{domain: d, entity: s})
		}
	}
	return out
}

// orderedFavorites is the favorites group in the order ui.json lists them: the
// order they were pinned in, changed by alt+arrows. An entity that is not in the
// current list - filtered away, or not one of ours any more - is simply skipped.
func (m *uiModel) orderedFavorites(matched []State) []State {
	byID := make(map[string]State, len(matched))
	for _, s := range matched {
		byID[s.EntityID] = s
	}
	out := make([]State, 0, len(m.cfg.Favorites))
	for _, id := range m.cfg.Favorites {
		if s, ok := byID[id]; ok {
			out = append(out, s)
		}
	}
	return out
}

func (m *uiModel) rowAt(i int) (row, bool) {
	rows := m.rows()
	if i < 0 || i >= len(rows) {
		return row{}, false
	}
	return rows[i], true
}

func (m *uiModel) selected() State {
	r, ok := m.rowAt(m.cursor)
	if !ok || r.isGroup {
		return State{}
	}
	return r.entity
}

// groupOrder lists the groups of the current view, in the order saved for it,
// then busiest first so the noisy ones sink.
func (m *uiModel) groupOrder(states []State) []string {
	counts := map[string]int{}
	for _, s := range states {
		counts[m.groupKey(s)]++
	}
	out := make([]string, 0, len(counts))
	for d := range counts {
		out = append(out, d)
	}
	order := m.cfg.orderFor(m.currentView())
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := rank(order, out[i]), rank(order, out[j]); ri != rj {
			return ri < rj
		}
		if m.currentView() == viewAreas {
			if (out[i] == "unknown") != (out[j] == "unknown") {
				return out[j] == "unknown"
			}
		}
		if counts[out[i]] != counts[out[j]] {
			return counts[out[i]] > counts[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// groupKey is what a group header is named for the current view. The default
// view groups by domain, the same thing the list has always done; "areas"
// groups by the entity's area, and "states" groups by the current state.
func (m *uiModel) groupKey(s State) string {
	switch m.currentView() {
	case viewAreas:
		return m.areaName(s)
	case viewStates:
		if s.State == "" {
			return "unknown"
		}
		return s.State
	case viewFlat:
		return "all"
	}
	return domain(s.EntityID)
}

// areaName resolves an entity's area: the mapping fetched from Home Assistant
// first, then an attribute an integration recorded for itself. "unknown" when
// the entity has no area, which is most of them.
func (m *uiModel) areaName(s State) string {
	if a := m.areas[s.EntityID]; a != "" {
		return a
	}
	for _, key := range []string{"area_name", "area", "area_id", "room", "device_area"} {
		if v, ok := s.Attributes[key].(string); ok && v != "" {
			// An attribute may carry an area id rather than a name.
			if name := m.areas[v]; name != "" {
				return name
			}
			return v
		}
	}
	return "unknown"
}

// currentView is the grouping mode as one of the known values. The field is a
// plain string so ui.json and a hand-written test can set it directly; anything
// unrecognised falls back rather than being rejected, because only a hand-edited
// file can produce one. One switch, and no re-parsing per entity.
func (m *uiModel) currentView() viewMode {
	switch viewMode(m.view) {
	case viewAreas, viewStates, viewFlat:
		return viewMode(m.view)
	}
	return viewGroups
}

// setView switches the grouping mode. An unknown name falls back to "groups"
// rather than refusing: the only caller that can pass garbage is a hand-edited
// ui.json, and the flag path validates before it gets here.
func (m *uiModel) setView(v string) {
	n, err := parseView(v)
	if err != nil {
		n = viewGroups
	}
	m.view = string(n)
}

// cycleView moves to the next grouping mode.
func (m *uiModel) cycleView() {
	modes := []viewMode{viewGroups, viewAreas, viewStates, viewFlat}
	for i, md := range modes {
		if md == m.currentView() {
			m.setView(string(modes[(i+1)%len(modes)]))
			return
		}
	}
	m.setView(string(modes[0]))
}

// cycleViewAndSelect switches grouping and stays on the same entity: every
// group around it is different after the switch, so its row index is not.
func (m *uiModel) cycleViewAndSelect() {
	was, _ := m.rowAt(m.cursor)
	m.cycleView()
	m.cfg.View = string(m.currentView())
	_ = saveUIConfig(m.cfg)
	m.follow(was)
	m.setStatus("view: "+string(m.currentView()), statusOK)
}

func (m *uiModel) move(d int) {
	n := len(m.rows())
	if n == 0 {
		m.cursor = 0
		return
	}
	m.cursor += d
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= n {
		m.cursor = n - 1
	}
}

// page moves a screenful, so a 130-row list does not need 130 presses.
func (m *uiModel) page(dir int) {
	n := m.listHeight()
	if n < 2 {
		n = 2
	}
	m.move(dir * (n - 1))
}

// firstEntity lands on the top entity row. Home is no use on a group header.
func (m *uiModel) firstEntity() { m.resetCursor() }

// lastEntity lands on the bottom entity row, for the same reason.
func (m *uiModel) lastEntity() {
	rows := m.rows()
	for i := len(rows) - 1; i >= 0; i-- {
		if !rows[i].isGroup {
			m.cursor = i
			return
		}
	}
	m.cursor = 0
}

// focusHeader puts the cursor on a group's header row, whichever way the group
// is currently folded.
func (m *uiModel) focusHeader(domain string) {
	for i, r := range m.rows() {
		if r.isGroup && r.domain == domain {
			m.cursor = i
			return
		}
	}
	m.keepCursorInRange()
}

// foldDown is what esc does once there is nothing left to close: it shuts the
// group the cursor is in, or the nearest unfolded one (above first, then below)
// if the cursor is sitting on an already-folded row, and then steps to the
// next still-open row. It never opens a group: walking down with repeated
// presses is what shuts the whole list.
func (m *uiModel) foldDown() {
	if r, ok := m.rowAt(m.cursor); ok && !m.isFolded(r.domain) {
		m.setFolded(r.domain, true)
		m.keepCursorInRange()
	} else {
		rows := m.rows()
		target := -1
		for dist := 1; dist <= len(rows); dist++ {
			for _, cand := range []int{m.cursor - dist, m.cursor + dist} {
				if cand < 0 || cand >= len(rows) {
					continue
				}
				if rows[cand].isGroup && !m.isFolded(rows[cand].domain) {
					target = cand
					break
				}
			}
			if target >= 0 {
				break
			}
		}
		if target >= 0 {
			m.setFolded(rows[target].domain, true)
			m.cursor = target
			m.keepCursorInRange()
		}
	}
	rows := m.rows()
	for i := m.cursor; i < len(rows); i++ {
		if !m.isFolded(rows[i].domain) {
			m.cursor = i
			return
		}
	}
	m.keepCursorInRange()
}

// afterMove is what every navigation key schedules: a re-read of the row the
// cursor landed on. While the websocket is subscribed there is nothing to ask
// for — the next event for that row is already on its way.
func (m *uiModel) afterMove() tea.Cmd {
	if m.conn != nil {
		return nil
	}
	return m.hoverCmd()
}

// frame is where the scrolling list sits in the terminal for one draw: the
// window into rows(), the screen line that window starts on, and the in-view
// group indicator above it when the list is scrolled past its own group header.
//
// Every field is derived from the model rather than stored. View used to write
// rowTop and winStart back into the model as a side effect and rowAtY read them
// out again, which put one piece of arithmetic in two places and left a click
// mapped to whatever the last View happened to have drawn.
type frame struct {
	start, end int    // the window into rows()
	rowTop     int    // screen line, counting from 1, that start is drawn on
	group      string // the in-view group indicator's name, "" when there is none
}

// frame computes the geometry of one draw from the list as View built it.
func (m *uiModel) frame(rows []row) frame {
	start, end := window(m.cursor, len(rows), m.listHeight())
	f := frame{start: start, end: end, rowTop: 3} // top rule, header, rule
	if m.showFilter() {
		f.rowTop = 4 // ... and the filter input above them
	}
	// The title, filter and rule above are drawn outside the window, so they
	// never scroll away. This keeps the *group* visible too, which the top rule
	// does not: scrolled deep into a 132-row group there would otherwise be no
	// indication of what you are looking at. The indicator takes one of the
	// list's own lines, so the rows below it move down by one.
	if start < len(rows) && !rows[start].isGroup {
		f.rowTop++
		f.group = rows[start].domain
	}
	return f
}

// ---------------------------------------------------------------- mouse

// handleMouse selects a row on click and scrolls on the wheel. Only active with
// --mouse, because enabling mouse tracking takes the pointer away from terminal
// text selection.
//
// Any movement closes an open sub-menu first: the menu belongs to the row it was
// opened on, and silently following the cursor would leave it pointing at a
// different entity.
func (m *uiModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Button == tea.MouseButtonWheelUp:
		m.closeMenu()
		m.move(-3)
		return m, m.afterMove()
	case msg.Button == tea.MouseButtonWheelDown:
		m.closeMenu()
		m.move(3)
		return m, m.afterMove()
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft:
		m.closeMenu()
		i, ok := m.rowAtY(msg.Y)
		if !ok {
			return m, nil
		}
		m.cursor = i
		return m, m.afterMove()
	}
	return m, nil
}

// rowAtY maps a screen line to a row, using the window this model draws now.
// A MouseMsg.Y counts from 1 while rowTop counts the lines above the list, so
// the first drawn row arrives as rowTop+1 - subtract one before indexing.
func (m *uiModel) rowAtY(y int) (int, bool) {
	rows := m.rows()
	f := m.frame(rows)
	if y <= f.rowTop {
		return 0, false
	}
	i := f.start + (y - f.rowTop - 1)
	if i < 0 || i >= len(rows) {
		return 0, false
	}
	return i, true
}

// resetCursor lands on the first entity row, not on a group header, so that
// typing a filter and immediately pressing space does something. With every
// group folded there is no entity row, so it falls back to the first group.
func (m *uiModel) resetCursor() {
	m.cursor = 0
	for i, r := range m.rows() {
		if !r.isGroup {
			m.cursor = i
			return
		}
	}
}

func (m *uiModel) keepCursorInRange() {
	if m.cursor < 0 {
		m.cursor = 0
	}
	if n := len(m.rows()); m.cursor >= n {
		m.cursor = n - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func dropLastRune(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return ""
	}
	return string(r[:len(r)-1])
}

// window picks the slice of rows to draw, keeping the cursor centred and
// clamped at both ends. Derived from state rather than stored, so it is always
// consistent with the cursor.
func window(cursor, n, height int) (int, int) {
	if height < 1 {
		height = 1
	}
	if n <= height {
		return 0, n
	}
	start := cursor - height/2
	if start < 0 {
		start = 0
	}
	if start+height > n {
		start = n - height
	}
	return start, start + height
}

// isActive is the "this is on" test used for colour and for group summaries.
func isActive(s State) bool {
	switch s.State {
	case "on", "open", "unlocked", "playing", "active", "home", "detected", "true":
		return true
	}
	return false
}

var (
	styleTitle  = lipgloss.NewStyle().Bold(true)
	styleDim    = lipgloss.NewStyle().Faint(true)
	styleOn     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleOff    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	styleSelect = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	styleGroup  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	styleOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleKey    = lipgloss.NewStyle().Foreground(lipgloss.Color("213"))
	styleRule   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	cursorBlock = lipgloss.NewStyle().Reverse(true).Render(" ")
)

func (m *uiModel) View() string {
	var b strings.Builder
	rows := m.rows()

	// What the list can reach, not how many rows are on screen: every run starts
	// with the groups closed, so counting visible entity rows would say 0 on
	// the frame that most needs the number.
	shown := len(m.matched())
	folded := 0
	for _, r := range rows {
		if r.isGroup && !m.expanded(r.domain) {
			folded++
		}
	}
	live := m.liveState()
	liveStyle := styleDim
	switch live {
	case "live":
		liveStyle = styleOn
	case "connecting", "polling":
		liveStyle = styleKey
	}
	w := m.termWidth()
	if w < 40 {
		w = 40
	}
	innerWidth := w - 4
	if innerWidth < 20 {
		innerWidth = 20
	}
	base := m.client.base
	// What the list is showing and what the house holds: on an instance where
	// the default view hides four fifths of the entities, "127/387" is the
	// difference between a short list and a missing one. Both numbers are
	// already computed for the list itself.
	counts := fmt.Sprintf("%d/%d", shown, len(m.states))
	if folded > 0 {
		counts += fmt.Sprintf(" · %d folded", folded)
	}
	tail := "  ·  " + counts + "  ·  " + live
	avail := innerWidth - len([]rune(tail)) - 2
	if avail < 8 {
		avail = 8
	}
	base = truncate(base, avail)

	header := "  " + styleTitle.Render(base) + styleDim.Render("  ·  ") +
		styleDim.Render(counts+"  ·  ") + liveStyle.Render(live)
	boxContent := header
	// A rule above and below turns the single line into a header band, so it
	// reads as a header rather than a stray line of text.
	b.WriteString("  " + styleRule.Render(strings.Repeat("─", m.ruleWidth()-2)) + "\n")
	b.WriteString(boxContent + "\n")
	if m.showFilter() {
		if m.searching {
			b.WriteString("  " + styleSelect.Render("filter") + " " + styleKey.Render("(searching)") + "  " + styleSelect.Render(m.filter) + cursorBlock + "\n")
		} else {
			b.WriteString("  " + styleDim.Render("filter (held)") + "  " + styleDim.Render(m.filter) + "\n")
		}
	}
	b.WriteString("  " + styleRule.Render(strings.Repeat("─", m.ruleWidth())) + "\n")

	if len(rows) == 0 {
		msg := "  no matches"
		if !m.ready {
			msg = "  loading…"
		}
		b.WriteString(styleDim.Render(msg) + "\n")
	}

	// Where the list sits in this frame. The frame carries the lines reserved
	// around it too, so the scroll window and the click mapping cannot disagree
	// about which rows are on screen.
	f := m.frame(rows)

	if f.group != "" {
		b.WriteString("  " + styleGroup.Render("▾ "+truncate(f.group, max(8, m.termWidth()-26))) +
			styleDim.Render(fmt.Sprintf("  (%d in view of %d)", f.end-f.start, rows[f.start].total)) + "\n")
	}

	for i := f.start; i < f.end; i++ {
		b.WriteString(m.line(rows[i], i == m.cursor) + "\n")
		if i == m.cursor {
			for _, l := range m.panelLines() {
				b.WriteString(l + "\n")
			}
		}
	}
	if f.end < len(rows) {
		b.WriteString(styleDim.Render(fmt.Sprintf("  … %d more", len(rows)-f.end)) + "\n")
	}

	b.WriteString("  " + styleRule.Render(strings.Repeat("─", m.ruleWidth())) + "\n")
	if m.status != "" {
		st := styleOK
		if m.kind == statusErr {
			st = styleErr
		}
		b.WriteString("  " + st.Render(m.clip(m.status, 2)) + "\n")
	}
	b.WriteString(styleDim.Render("  " + m.clip(m.helpLine(), 2)))
	if box := m.helpPopup(); box != nil {
		return overlay(b.String(), box, m.termWidth(), m.height)
	}
	return b.String()
}

// liveState names where updates are coming from, because a list that silently
// stopped updating is the worst thing this program can do.
func (m *uiModel) liveState() string {
	switch {
	case m.conn != nil:
		return "live"
	case m.wantWS && !m.polling:
		return "connecting"
	case m.interval > 0:
		return "polling"
	default:
		return "static"
	}
}

// panelHeight is what a panel (the action sub-menu, the details) may take from
// the scrolling body: half of it. The list keeps the rest, so the row you are
// looking at stays visible in context and the frame can never overflow — the
// panel and the rows are drawn into the same space, so their sizes have to be
// decided against each other rather than independently.
// showFilter reports whether the filter input is drawn. It stays visible
// whenever there is text in it or while a search is in progress, so the
// user can refine; an idle, empty screen shows no input at all.
func (m *uiModel) showFilter() bool {
	return m.searching || m.filter != ""
}

// chromeLines counts the lines View draws outside the scrolling list. It drops
// one when the filter input is hidden, matching what the method writes.
//
// top rule(1) + header(1) + filter(1) + group indicator(1) + "… more"(1) +
// closing rule(1) + status(1) + help(1); the filter line is optional. The
// group indicator and "… more" only appear when the list is scrolled or
// overflows, so this is the worst case and the view can never outgrow the
// terminal.
func (m *uiModel) chromeLines() int {
	n := 9
	if !m.showFilter() {
		n = n - 1
	}
	return n
}

func (m *uiModel) panelHeight() int { return max(1, (m.height-m.chromeLines())/2) }

// listHeight is the room left for rows once the frame and any panel are paid
// for. Must match what chromeLines() counts. It is clamped at one so a short
// terminal cannot squeeze the list to nothing while two panels still draw.
func (m *uiModel) listHeight() int {
	body := m.height - m.chromeLines()
	return max(1, body-min(len(m.panelLines()), m.panelHeight()))
}

// termWidth is the terminal width, or defaultWidth before the first
// WindowSizeMsg. Tests that do not set width get the same layout they always
// had, and no terminal width means nothing is clipped or budgeted.
func (m *uiModel) termWidth() int {
	if m.width <= 0 {
		return defaultWidth
	}
	return m.width
}

// clip shortens a rendered line to what the terminal can show, counting runes.
// It is a no-op when the width is unknown, so a test-built model keeps its
// natural layout. Called on plain text, never on a styled string.
func (m *uiModel) clip(s string, indent int) string {
	return truncate(s, max(8, m.termWidth()-indent))
}

// ruleWidth is how wide the horizontal rules may be. The old hardcoded 72
// wrapped on a 60-column terminal, and a wrapped rule is a lost line: the
// frame grows past the height and bubbletea keeps the last height lines, so
// the title and filter scroll off the top.
func (m *uiModel) ruleWidth() int {
	return max(16, m.termWidth()-4)
}

// panelLines is the one panel that draws under the cursor row. The sub-menu
// wins when both are open, which keeps the frame arithmetic honest: each panel
// is sized against half the body, so two at once would each take a half and
// leave the list nothing. Losing the menu to ^D and back is free, because no
// state is dropped — the details are still there when the menu closes.
func (m *uiModel) panelLines() []string {
	if l := m.menuLines(); l != nil {
		return l
	}
	return m.detailLines()
}

// helpLine is the bottom line: what is true right now, and what to press about
// it. Full shortcuts are available by pressing '?'.
func (m *uiModel) helpLine() string {
	if m.searching {
		return "search mode · type to filter · enter or esc leaves · ^C quit"
	}
	if m.menu {
		return "menu mode · ↑↓ choose · 1-9 run · esc close · ^C quit"
	}
	if m.help {
		return "? or esc closes help · ^C quit"
	}
	return "? help · / search · space act · v view · enter actions · ^C quit"
}

func (m *uiModel) line(r row, selected bool) string {
	pointer := "  "
	nameStyle := styleDim
	if selected {
		pointer = styleSelect.Render("▸ ")
		nameStyle = styleSelect
	}

	if r.isGroup {
		marker := "▸"
		if m.expanded(r.domain) {
			marker = "▾"
		}
		summary := fmt.Sprintf("(%d)", r.total)
		if r.on > 0 {
			summary += " " + styleOn.Render(fmt.Sprintf("%d on", r.on))
		}
		// No position marker: the header used to carry "·3" to say a group was
		// pinned to the top, which was worth showing when only a few were. Now
		// group_order holds every domain, so every group got a number and
		// "light ·6" said nothing the order of the list does not already say.
		// The favorites group is the one header that is not a domain, an area or
		// a state, so it keeps its star and says what it is.
		name := r.domain
		if r.domain == favoritesGroup {
			name = "★ " + name
		}
		return fmt.Sprintf(" %s%s %s%s",
			pointer, styleGroup.Render(marker),
			nameStyle.Render(truncate(name, max(8, m.termWidth()-30))),
			styleDim.Render(" "+summary))
	}

	s := r.entity
	stateStyle := styleOff
	if isActive(s) {
		stateStyle = styleOn
	}
	acts := m.reg.actions(domain(s.EntityID))
	if len(acts) == 0 {
		stateStyle = styleDim // nothing to do with it
	}
	if selected {
		stateStyle = nameStyle
	}
	// An entity with no friendly_name has no name to show, so the name cell
	// stays empty and the id stands in the id column - exactly as `has list`
	// renders it. Falling back to the id inside the name column put ids at two
	// different indents and made light.workspace_light read like a name.
	name, id := friendlyName(&s), "  "+s.EntityID
	if m.cfg.isFavorite(s.EntityID) {
		name = "★ " + name
	}
	// Only the highlighted row advertises its actions, so the list stays calm.
	hint := ""
	if selected && len(acts) > 0 {
		hint = fmt.Sprintf(" %d acts", len(acts))
	}
	// The row has to fit the terminal. After the fixed columns (indent,
	// pointer, state, gaps) the remainder is split between the name, the id
	// and the hint, in that order of priority: the name shrinks first, then
	// the id, and the hint is last.
	budget := m.termWidth() - entityGutter
	if budget < 16 {
		budget = 16
	}
	nameW := min(nameWidth, max(8, budget-len(hint)-6)) // 6 = a readable id
	idW := budget - nameW - len(hint)
	idStr := ""
	if idW > 0 {
		idStr = styleDim.Render(truncate(id, idW))
	}
	line := fmt.Sprintf("   %s %s %s%s",
		pointer,
		stateStyle.Render(pad(s.State, stateWidth)),
		nameStyle.Render(pad(name, nameW)),
		idStr)
	return line + styleKey.Render(hint)
}

// menuLines renders the action sub-menu under the highlighted row.
func (m *uiModel) menuLines() []string {
	if !m.menu {
		return nil
	}
	acts := m.actionsFor(m.cursor)
	if len(acts) == 0 {
		return nil
	}
	out := []string{styleDim.Render("       actions for " + m.selected().EntityID + " (menu)")}
	// A domain with a dozen actions must not swallow the list either.
	room := m.panelHeight() - 2
	for i, a := range acts {
		if i >= room {
			out = append(out, styleDim.Render(fmt.Sprintf("       … %d more", len(acts)-i)))
			break
		}
		mark := "  "
		style := styleDim
		if i == m.menuIdx {
			mark = styleKey.Render("▸ ")
			style = styleKey
		}
		key := strconv.Itoa(i + 1)
		if a.local {
			// Nothing to call, so only the label and the digit: the service
			// column would be a lie.
			out = append(out, "     "+mark+style.Render(pad(a.label, 12))+"  "+
				styleDim.Render(truncate("("+key+")", max(8, m.termWidth()-22))))
			continue
		}
		name := fmt.Sprintf("%s  (%s)", a.service, key)
		out = append(out, "     "+mark+style.Render(pad(a.label, 12))+"  "+
			styleDim.Render(truncate(name, max(8, m.termWidth()-22))))
	}
	out = append(out, styleDim.Render("       ↑↓ or 1-9 choose · enter/space run · esc close"))
	return out
}

// detailAttributes are the attributes that say nothing, plus battery, which is
// hoisted to the top of the panel instead.
var detailAttributes = map[string]bool{
	"friendly_name":      true, // already the row's name
	"icon":               true, // a class name, not information
	"entity_picture":     true, // a URL
	"supported_features": true, // a bitmask number no human reads
	"hidden":             true, // an internal flag
	"battery_level":      true, // shown first, as "battery"
}

// detailLines renders the highlighted entity's attributes under its row, the
// same place the action sub-menu goes. Trimmed to what the list can spare, so
// an entity with fifty attributes cannot push the header off the screen.
func (m *uiModel) detailLines() []string {
	if !m.details {
		return nil
	}
	s := m.selected()
	if s.EntityID == "" {
		return nil
	}
	keys := make([]string, 0, len(s.Attributes))
	for k := range s.Attributes {
		if !detailAttributes[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	out := []string{styleDim.Render("       " + s.EntityID + " · " + pad(s.State, 13) +
		" changed " + timestamp(s.LastChanged))}
	// The budget is half the body: listHeight already subtracts this panel, and
	// asking it here would be a loop.
	room := m.panelHeight() - 1
	// Battery first, when there is one. It is the attribute people open this
	// panel to see, and buried in an alphabetical dump of twenty others it is
	// the one they do not find.
	if b, ok := s.Attributes["battery_level"]; ok {
		out = append(out, "       "+styleKey.Render(pad("battery", 22))+batteryText(b))
	}
	for i, k := range keys {
		if i >= room {
			out = append(out, styleDim.Render(fmt.Sprintf("       … %d more attributes", len(keys)-i)))
			break
		}
		out = append(out, "       "+styleKey.Render(pad(k, 22))+styleDim.Render(attributeValue(s.Attributes[k])))
	}
	out = append(out, styleDim.Render("       ^D or esc closes"))
	return out
}

// batteryText renders a battery level the way it is meant to be read. Home
// Assistant reports a percentage, but an integration sending a fraction should
// not be shown as 1%.
func batteryText(v any) string {
	switch n := v.(type) {
	case float64:
		if n >= 0 && n <= 100 {
			return fmt.Sprintf("%.0f%%", n)
		}
		return fmt.Sprint(n)
	case int:
		return fmt.Sprintf("%d%%", n)
	default:
		return attributeValue(v)
	}
}

// attributeValue prints one attribute the way a person reads it: strings bare,
// lists and objects as compact JSON, everything else as-is.
func attributeValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case []any, map[string]any:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	default:
		return fmt.Sprint(v)
	}
}

// helpPopup is the full keyboard shortcut reference. It is drawn as a box over
// the list, not inside it: as a panel it was spliced in under the cursor row,
// so on a group header it read as a help screen the group had swallowed and
// every row below it jumped. An overlay costs the list nothing.
func (m *uiModel) helpPopup() []string {
	if !m.help {
		return nil
	}
	// Replaced rather than added: an 18-entry box is truncated to "… 2 more" on a
	// 24-line terminal, so a key added here is a key nobody sees.
	items := []struct {
		key  string
		desc string
	}{
		{"/", "search / filter entities"},
		{"space", "primary action (toggle / turn on)"},
		{"enter", "open action sub-menu (or toggle group)"},
		{"1-9", "run that action while the sub-menu is open"},
		{"f", "favorite / pin entity to top"},
		{"v", "cycle grouping (groups · areas · states · flat)"},
		{"^D", "toggle entity attributes panel"},
		{"↑ / ↓", "navigate entities and groups"},
		{"pgup/pgdn", "move a screenful · home/end first and last"},
		{"← / →", "fold / expand group"},
		{"⌥↑/↓", "move a group, or a favorite, one place up or down"},
		{"^A", "toggle read-only entities in the list"},
		{"^U / ^R", "clear the filter / refresh"},
		{"?", "close this help"},
		{"esc", "close panel / clear filter / fold group"},
		{"^C", "quit"},
	}

	// A box taller than the terminal would push the frame past m.height and
	// scroll the header off the top, so the list of keys is trimmed to what
	// the box can hold. room pays for the two borders and the title. How to
	// close it is the help line under the box, which is never covered.
	descW := max(8, m.termWidth()-21)
	room := max(1, m.height-3)
	out := []string{styleTitle.Render("keyboard shortcuts:")}
	for i, it := range items {
		if i >= room {
			out = append(out, styleDim.Render(fmt.Sprintf("   … %d more", len(items)-i)))
			break
		}
		out = append(out, "   "+styleKey.Render(pad(it.key, 12))+"  "+styleDim.Render(truncate(it.desc, descW)))
	}
	return out
}

// overlay draws box in the middle of an already-rendered view. It replaces
// lines rather than adding them: an overlay that grew the view would be one
// line too tall, and bubbletea answers that by scrolling the title and filter
// off the top. Over-long rows fall off the bottom, which is what a popup on a
// short terminal should do.
func overlay(view string, box []string, width, height int) string {
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	// The box is centred in the terminal, not in whatever the list happened to
	// draw. With every group closed the view is seven lines, and a box centred
	// in seven loses its own bottom half. Padding to the terminal height draws
	// nothing new - bubbletea blanks those lines anyway - and cannot push the
	// header off the top, because the view never grows past m.height.
	for len(lines) < height {
		lines = append(lines, "")
	}
	inner := 0
	for _, l := range box {
		inner = max(inner, lipgloss.Width(l))
	}
	// │ + space + space + │, plus room for the centered inset.
	inner = min(inner, max(4, width-6))
	edge := styleRule.Render("│")
	frame := []string{styleRule.Render("┌" + strings.Repeat("─", inner+2) + "┐")}
	for _, l := range box {
		frame = append(frame, edge+" "+padTo(l, inner)+" "+edge)
	}
	frame = append(frame, styleRule.Render("└"+strings.Repeat("─", inner+2)+"┘"))

	left := strings.Repeat(" ", max(0, (width-inner-4)/2))
	top := max(0, (len(lines)-len(frame))/2)
	for i, l := range frame {
		if y := top + i; y < len(lines) {
			lines[y] = left + l
		}
	}
	return strings.Join(lines, "\n")
}

// padTo pads a styled string to n visible columns. pad() counts runes, so on a
// coloured line it would count the escape codes as text.
func padTo(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}
