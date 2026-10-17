package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// uiConfig is ~/.config/has/ui.json: how the UI is arranged, not what it talks
// to. No secrets, so it is world-readable unlike config.json.
//
// Four preferences, no snapshots: the order of the groups in each of the three
// groupable views, the entities you favorited, and the view you left it in. Fold
// state is not saved — every run starts closed — and there is no hidden list,
// because folding already does that job for a whole domain.
type uiConfig struct {
	// GroupOrder is the domains top to bottom. It is written the first time a
	// group is moved, and holds every domain at that point, not only the ones
	// that were pinned to the top: a partial order would make "move down" mean
	// nothing. Anything not listed — a domain added since — sorts below.
	GroupOrder []string `json:"group_order,omitempty"`
	// AreaOrder is the same thing for the areas view, and StateOrder for the
	// states view. One per view, because the names live in different spaces: an
	// area order saved as a domain order would bury the domain one.
	AreaOrder  []string `json:"area_order,omitempty"`
	StateOrder []string `json:"state_order,omitempty"`
	// Favorites is the entity ids the user pinned to the top group, in the order
	// they want them - which is the order they were pinned in, changed by
	// alt+arrows. It lives here precisely because it is not a secret and should
	// outlive a run.
	Favorites []string `json:"favorites,omitempty"`
	// View persists the grouping view (groups, areas, states, flat).
	View string `json:"view,omitempty"`
}

// orderFor is the saved order for a view, and withOrder puts a new one back.
// Together they keep the field-per-view detail in one place, so no caller has to
// know that "areas" means AreaOrder, and no caller can drop the others: both copy
// rather than construct, which is the rule that keeps a field added later from
// going missing on the next write.
func (c uiConfig) orderFor(view viewMode) []string {
	switch view {
	case viewAreas:
		return c.AreaOrder
	case viewStates:
		return c.StateOrder
	default:
		return c.GroupOrder
	}
}

func (c uiConfig) withOrder(view viewMode, order []string) uiConfig {
	next := c
	switch view {
	case viewAreas:
		next.AreaOrder = order
	case viewStates:
		next.StateOrder = order
	default:
		next.GroupOrder = order
	}
	return next
}

func uiConfigPath() (string, error) {
	base, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "ui.json"), nil
}

func loadUIConfig() (uiConfig, error) {
	var c uiConfig
	p, err := uiConfigPath()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

func saveUIConfig(c uiConfig) error {
	p, err := uiConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWriteFile(p, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Chmod(p, 0o644)
}

// isFavorite is a small, safe lookup. Favorites are by entity id, which is
// the same key every other command already uses.
func (c uiConfig) isFavorite(id string) bool {
	return slices.Contains(c.Favorites, id)
}

// toggleFavorite returns a copy with one entity added or removed from the
// favorites list. It copies first so a field added later cannot be dropped.
func (c uiConfig) toggleFavorite(id string) uiConfig {
	next := c
	next.Favorites = append([]string(nil), c.Favorites...)
	if i := slices.Index(next.Favorites, id); i >= 0 {
		next.Favorites = append(next.Favorites[:i], next.Favorites[i+1:]...)
	} else {
		next.Favorites = append(next.Favorites, id)
	}
	return next
}

// rank is a group's position in a saved order; lower comes first. A group that
// is not in the file — one added to the house since the order was written —
// sorts below every group that is, busiest first as usual.
func rank(order []string, group string) int {
	if i := slices.Index(order, group); i >= 0 {
		return i
	}
	return len(order)
}
