//go:build windows && amd64

// Package ui renders the governor's live view-model. The actual Fyne app lives
// in fyne.go and is compiled by default; Fyne needs CGO + a C toolchain, so a
// "nogui" build tag drops it for CGO-free/CI builds. In such a build runner is
// nil, Run reports that the GUI was not compiled in, and the caller falls back
// to the text reporter.
package ui

import (
	"errors"
	"fmt"
	"strings"

	"ThreadOrchestra/governor"
)

// runner is installed by the Fyne implementation's init, unless built -tags nogui.
var runner func(*governor.Governor) error

// Available reports whether the GUI was compiled in (i.e. not a -tags nogui build).
func Available() bool { return runner != nil }

// Run drives the GUI for one game session, blocking until the window closes.
// It must be called from the main goroutine (Fyne's requirement).
func Run(g *governor.Governor) error {
	if runner == nil {
		return errors.New("GUI not compiled in: this is a -tags nogui build; rebuild without it (needs CGO + a C toolchain), or run with -nogui")
	}
	return runner(g)
}

// column describes one thread-table column. Shared by the Fyne table so header
// and cell rendering stay in lockstep. The set is deliberately lean — the
// governor's job is to show what it decided, not mirror a process explorer.
type column struct {
	title string
	width float32
}

var columns = []column{
	{"TID", 70},
	{"Thread", 190},
	{"Role", 120},
	{"Cyc/s", 80},
	{"Sw/s", 70},
	{"Prio", 55},
	{"Bucket", 110},
	{"Action", 170},
}

// identity is the human-facing name for a thread: its game-set description,
// else its resolved module (full mode), else a dash. It never surfaces a raw
// start address — those are noise (and zero under Overwatch's anti-tamper).
func identity(r governor.ThreadRow) string {
	if r.Name != "" {
		return r.Name
	}
	if r.Module != "" {
		return r.Module
	}
	return "—"
}

// notable reports whether a thread is worth showing by default. The bulk of a
// game's threads are a parked worker pool that the governor demotes en masse;
// listing all 60+ of them buries the handful that matter. A thread earns a row
// when it's in a bucket the governor actively steers, is CPU-starved, or has a
// real name/module — everything else collapses behind the "show all" toggle.
func notable(r governor.ThreadRow) bool {
	switch r.Bucket {
	case "critical", "interactive", "untouchable":
		return true
	}
	return r.Starved || r.Name != "" || r.Module != ""
}

// visibleRows returns the rows to display: the full set when showAll, otherwise
// just the notable ones. The input is already sorted by cycle rate.
func visibleRows(rows []governor.ThreadRow, showAll bool) []governor.ThreadRow {
	if showAll {
		return rows
	}
	out := make([]governor.ThreadRow, 0, len(rows))
	for _, r := range rows {
		if notable(r) {
			out = append(out, r)
		}
	}
	return out
}

// cellText formats one thread-table cell for the given row/column.
func cellText(rows []governor.ThreadRow, row, col int) string {
	if row < 0 || row >= len(rows) {
		return ""
	}
	r := rows[row]

	switch col {
	case 0:
		return fmt.Sprintf("%d", r.TID)
	case 1:
		return identity(r)
	case 2:
		return dashIfEmpty(r.Role)
	case 3:
		return fmt.Sprintf("%.2fM", r.CyclesRate/1e6)
	case 4:
		return fmt.Sprintf("%.0f", r.SwitchRate)
	case 5:
		return fmt.Sprintf("%d", r.Priority)
	case 6:
		bucket := r.Bucket
		if r.Starved {
			bucket += " ⚠"
		}
		return bucket
	case 7:
		return dashIfEmpty(r.Applied)
	default:
		return ""
	}
}

// titleText is the headline line: game, PID, phase, and the governor's mode.
func titleText(view governor.ViewModel) string {
	if view.GameName == "" {
		return "ThreadOrchestra — waiting for first sample…"
	}
	focus := ""
	if !view.Focused {
		focus = "  ·  ⏸ unfocused (metrics paused)"
	}
	return fmt.Sprintf("%s  ·  pid %d  ·  %s  ·  %s / %s%s",
		view.GameName, view.PID, view.Phase, view.Optimisation, view.Aggression, focus)
}

// metricsText is the quieter second line: live totals and access mode.
func metricsText(view governor.ViewModel) string {
	if view.GameName == "" {
		return ""
	}
	return fmt.Sprintf("%d threads  ·  %.1fM cyc/s  ·  %.1f MB/s read  ·  %s mode",
		view.ThreadCount, view.TotalCycles/1e6, view.ReadRate/1e6, view.AccessMode)
}

// bucketSummary counts how each thread was classified — an at-a-glance picture
// of what the governor is doing without reading the table.
func bucketSummary(view governor.ViewModel) string {
	var critical, interactive, background, untouchable, unclassified int
	for _, r := range view.Rows {
		switch r.Bucket {
		case "critical":
			critical++
		case "interactive":
			interactive++
		case "background":
			background++
		case "untouchable":
			untouchable++
		default:
			unclassified++
		}
	}
	if view.GameName == "" {
		return ""
	}
	return fmt.Sprintf("critical %d    interactive %d    background %d    untouchable %d    unclassified %d",
		critical, interactive, background, untouchable, unclassified)
}

// warningsText joins any capability/privilege warnings for display.
func warningsText(view governor.ViewModel) string {
	if len(view.Warnings) == 0 {
		return ""
	}
	return "⚠ " + strings.Join(view.Warnings, "   ⚠ ")
}

// hiddenText describes how many threads the notability filter is collapsing.
func hiddenText(view governor.ViewModel, shown int) string {
	hidden := len(view.Rows) - shown
	if hidden <= 0 {
		return fmt.Sprintf("%d threads", len(view.Rows))
	}
	return fmt.Sprintf("%d of %d threads  ·  %d idle background hidden", shown, len(view.Rows), hidden)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
