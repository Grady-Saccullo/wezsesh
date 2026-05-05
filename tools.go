//go:build wezsesh_deps_anchor

package wezseshdepsanchor

import (
	_ "charm.land/bubbles/v2"
	_ "charm.land/bubbletea/v2"
	_ "charm.land/huh/v2"
	_ "charm.land/lipgloss/v2"
	_ "github.com/charmbracelet/x/ansi"
	_ "github.com/mattn/go-runewidth"
	_ "github.com/sahilm/fuzzy"
	_ "go.uber.org/goleak"
	_ "golang.org/x/sys/unix"
	_ "golang.org/x/text/unicode/norm"
)
