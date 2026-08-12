// Package pathutil holds path helpers shared by discovery and rendering.
//
// It exists to break a cycle: discover's worktree dedupe reports the canonical
// repo's path in a skip detail, and render prints paths in the inventory table.
// Both want the same "~" collapsing. In the Observatory agent this was one
// function in render.go that discover.go reached across the package boundary —
// legal there only because everything was package main.
package pathutil

import (
	"os"
	"path/filepath"
	"strings"
)

// Shorten renders an absolute path with the home directory as "~", which is how
// the user thinks about their own machine.
func Shorten(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == home {
		return "~"
	}
	if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}
	return p
}

// ExpandHome resolves a leading "~" in a user-supplied path. Only the "~" and
// "~/..." forms are handled: "~otheruser" is deliberately left alone rather than
// guessed at.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}
