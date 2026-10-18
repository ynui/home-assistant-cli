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
// to. No secrets. Four preferences, no snapshots: group orders, favorites, and
// the view. Fold state is not saved (every run starts closed), and there is no
// hidden list because folding already covers that for a whole domain.
type uiConfig struct {
	// GroupOrder is the domains top to bottom, all of them (a partial order
	// makes "move down" mean nothing). Anything not listed sorts below.
	GroupOrder []string `json:"group_order,omitempty"`
	// AreaOrder is the same for the areas view, StateOrder for the states
	// view: one per view, because the names live in different spaces.
	AreaOrder  []string `json:"area_order,omitempty"`
	StateOrder []string `json:"state_order,omitempty"`
	// Favorites is the pinned entity ids, in the order the user wants them.
	Favorites []string `json:"favorites,omitempty"`
	// View persists the grouping view (groups, areas, states, flat).
	View string `json:"view,omitempty"`
}

// orderFor / withOrder keep the field-per-view detail in one place. Both copy
// rather than construct, so writing one view's order cannot drop the others.
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

// isFavorite looks the id up in the favorites list.
func (c uiConfig) isFavorite(id string) bool {
	return slices.Contains(c.Favorites, id)
}

// toggleFavorite returns a copy with one entity added to, or removed from, the
// favorites. It copies first so a field added later cannot be dropped.
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

// rank is a group's position in a saved order; a group that is not in the
// file sorts below every group that is.
func rank(order []string, group string) int {
	if i := slices.Index(order, group); i >= 0 {
		return i
	}
	return len(order)
}
