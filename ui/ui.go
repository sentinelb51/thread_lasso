//go:build windows && amd64

// Package ui renders the governor's live view-model. The actual Fyne app lives
// in fyne.go and is compiled by default; Fyne needs CGO + a C toolchain, so a
// "nogui" build tag drops it for CGO-free/CI builds. In such a build runner is
// nil, Run reports that the GUI was not compiled in, and the caller falls back
// to the text reporter.
//
// Everything in this file is deliberately Fyne-free: it is the layout and
// formatting logic, and it has to keep compiling without CGO.
package ui

import (
	"errors"
	"fmt"
	"image/color"

	"ThreadOrchestra/governor"
)

// Event is one transition in the supervisor's lifecycle. Exactly one of the
// two payloads is meaningful: a non-nil Session means a game was found and the
// dashboard should attach to it; otherwise Status describes what is being
// waited on.
type Event struct {
	Status  string
	Session *governor.Governor
	Fatal   bool // the supervisor has given up; Status is the reason
}

// Feed is everything the GUI consumes for the life of the process.
type Feed struct {
	Events   <-chan Event
	Watching []string // configured executables, listed on the waiting screen
	OnQuit   func()   // cancels the supervisor when the window closes
}

// runner is installed by the Fyne implementation's init, unless built -tags nogui.
var runner func(Feed) error

// Available reports whether the GUI was compiled in (i.e. not a -tags nogui build).
func Available() bool { return runner != nil }

// Run drives the GUI for the life of the process, blocking until the window
// closes. It must be called from the main goroutine (Fyne's requirement).
func Run(feed Feed) error {
	if runner == nil {
		return errors.New("GUI not compiled in: this is a -tags nogui build; rebuild without it (needs CGO + a C toolchain), or run with -nogui")
	}
	return runner(feed)
}

// textAlign is the Fyne-free stand-in for fyne.TextAlign; fyne.go maps it.
type textAlign int

const (
	alignLeading textAlign = iota
	alignTrailing
)

// column describes one thread-table column. Shared by the Fyne table so header
// and cell rendering stay in lockstep. The set is deliberately lean — the
// governor's job is to show what it decided, not mirror a process explorer.
//
// chars is a truncation budget: cells are drawn as canvas text, which does not
// clip itself, so anything longer has to be shortened before it runs into the
// next column.
type column struct {
	title string
	width float32
	chars int
	align textAlign
}

// Thread leads because it is the column you read first; TID follows it for
// cross-referencing against a process explorer.
var columns = []column{
	{"Thread", 250, 32, alignLeading},
	{"TID", 70, 8, alignLeading},
	{"Role", 120, 15, alignLeading},
	{"Cyc/s", 90, 11, alignTrailing},
	{"Sw/s", 70, 8, alignTrailing},
	{"Prio", 60, 7, alignTrailing},
	{"Bucket", 120, 15, alignLeading},
	{"Action", 170, 22, alignLeading},
}

// Palette. Fixed colours rather than theme lookups: these are semantic (how
// hot, how protected), and they have to stay distinguishable from each other
// in both the light and the dark theme, which a foreground-derived colour
// would not.
var (
	colourCritical    = color.NRGBA{R: 0xE1, G: 0x5A, B: 0x5A, A: 0xFF}
	colourInteractive = color.NRGBA{R: 0xE0, G: 0x9B, B: 0x3D, A: 0xFF}
	colourBackground  = color.NRGBA{R: 0x6C, G: 0x8B, B: 0xA8, A: 0xFF}
	colourUntouchable = color.NRGBA{R: 0x9B, G: 0x82, B: 0xC9, A: 0xFF}
	colourMuted       = color.NRGBA{R: 0x8A, G: 0x93, B: 0x9E, A: 0xFF}
	colourWarning     = color.NRGBA{R: 0xE0, G: 0x9B, B: 0x3D, A: 0xFF}
	colourGood        = color.NRGBA{R: 0x5C, G: 0xB8, B: 0x7A, A: 0xFF}
)

// roleStyle is a role's marker and colour. The glyphs are all from Geometric
// Shapes (U+25xx), the block with the widest font coverage — an icon that
// renders as a missing-glyph box on someone else's machine is worse than no
// icon.
type roleStyle struct {
	icon   string
	colour color.NRGBA
}

var roleStyles = map[string]roleStyle{
	"main/sim":      {"◆", color.NRGBA{R: 0x4F, G: 0xC3, B: 0xF7, A: 0xFF}},
	"render":        {"▲", color.NRGBA{R: 0xB3, G: 0x93, B: 0xEB, A: 0xFF}},
	"gpu-driver":    {"▼", color.NRGBA{R: 0x7C, G: 0x9E, B: 0xEB, A: 0xFF}},
	"audio":         {"●", color.NRGBA{R: 0x5C, G: 0xB8, B: 0x7A, A: 0xFF}},
	"input":         {"■", color.NRGBA{R: 0x35, G: 0xBD, B: 0xC7, A: 0xFF}},
	"network/voice": {"◇", color.NRGBA{R: 0x4C, G: 0xA3, B: 0xE8, A: 0xFF}},
	"job-worker":    {"▪", color.NRGBA{R: 0xE0, G: 0x9B, B: 0x3D, A: 0xFF}},
	"loader":        {"▸", color.NRGBA{R: 0xE0, G: 0x7B, B: 0x53, A: 0xFF}},
	"telemetry":     {"▫", color.NRGBA{R: 0x8A, G: 0x93, B: 0x9E, A: 0xFF}},
	"pool-idle":     {"○", color.NRGBA{R: 0x6C, G: 0x77, B: 0x85, A: 0xFF}},
	"unknown":       {"·", color.NRGBA{R: 0x8A, G: 0x93, B: 0x9E, A: 0xFF}},
}

// roleOrder fixes the order the groups appear in, hottest responsibility
// first. It is a static list rather than a sort on measured cycles so that a
// group never jumps position between repaints.
var roleOrder = []string{
	"main/sim", "render", "gpu-driver", "audio", "input",
	"network/voice", "job-worker", "loader", "telemetry", "pool-idle", "unknown",
}

func styleForRole(role string) roleStyle {
	if style, ok := roleStyles[role]; ok {
		return style
	}
	return roleStyle{"·", colourMuted}
}

func colourForBucket(bucket string) color.NRGBA {
	switch bucket {
	case "critical":
		return colourCritical
	case "interactive":
		return colourInteractive
	case "background":
		return colourBackground
	case "untouchable":
		return colourUntouchable
	default:
		return colourMuted
	}
}

// displayRow is one line of the table: either a role group header or a thread.
type displayRow struct {
	header bool
	role   string
	count  int     // header only: threads in the group
	cycles float64 // header only: the group's combined cycle rate
	thread governor.ThreadRow
}

// notableCycleRate is the "doing something" line, roughly 0.02% of one modern
// core. Below it a thread woke up, checked a queue and went back to sleep.
const notableCycleRate = 1e6

// notable reports whether a thread is worth showing by default. The bulk of a
// game's threads are a parked worker pool that the governor demotes en masse;
// listing all 60+ of them buries the handful that matter. A thread earns a row
// when it's in a bucket the governor actively steers, is CPU-starved, has been
// tuned, is burning measurable CPU, or the game bothered to name it.
//
// A resolved module deliberately does not qualify. It used to, back when
// almost nothing resolved; now that entry points survive a scrubbed
// Win32StartAddress, every thread has one and the filter would pass all of
// them.
func notable(r governor.ThreadRow) bool {
	switch r.Bucket {
	case "critical", "interactive", "untouchable":
		return true
	}
	return r.Starved || r.Name != "" || r.Applied != "" || r.CyclesRate >= notableCycleRate
}

// buildRows groups the view's threads by role and flattens the result into the
// table's row list, one header per non-empty group. Input order (cycle rate
// descending) is preserved within each group.
func buildRows(rows []governor.ThreadRow, showAll bool) []displayRow {
	grouped := make(map[string][]governor.ThreadRow, len(roleOrder))
	for _, r := range rows {
		if !showAll && !notable(r) {
			continue
		}
		role := r.Role
		if role == "" {
			role = "unknown"
		}
		grouped[role] = append(grouped[role], r)
	}

	// Any role the classifier grows that roleOrder hasn't been told about still
	// gets a group, appended after the known ones, rather than vanishing.
	order := make([]string, 0, len(grouped))
	seen := make(map[string]bool, len(roleOrder))
	for _, role := range roleOrder {
		if len(grouped[role]) > 0 {
			order = append(order, role)
			seen[role] = true
		}
	}
	for role := range grouped {
		if !seen[role] {
			order = append(order, role)
		}
	}

	out := make([]displayRow, 0, len(rows)+len(order))
	for _, role := range order {
		members := grouped[role]
		cycles := 0.0
		for _, m := range members {
			cycles += m.CyclesRate
		}
		out = append(out, displayRow{header: true, role: role, count: len(members), cycles: cycles})
		for _, m := range members {
			out = append(out, displayRow{role: role, thread: m})
		}
	}
	return out
}

// cell is one rendered table cell: what to draw and how. A nil colour means
// "whatever the theme calls foreground", so the columns that carry no semantic
// colour follow the user's light/dark preference; muted picks the theme's
// dimmer tone for the supporting columns.
type cell struct {
	text   string
	colour color.Color
	muted  bool
	bold   bool
	align  textAlign
}

// cellFor formats one table cell. Header rows carry the group's aggregates in
// the columns where they mean the same thing as the per-thread value.
func cellFor(rows []displayRow, row, col int) cell {
	if row < 0 || row >= len(rows) || col < 0 || col >= len(columns) {
		return cell{muted: true}
	}
	r := rows[row]
	align := columns[col].align

	if r.header {
		style := styleForRole(r.role)
		switch col {
		case 0:
			return cell{text: fmt.Sprintf("%s  %s  ×%d", style.icon, r.role, r.count), colour: style.colour, bold: true}
		case 3:
			return cell{text: formatCycles(r.cycles), colour: style.colour, bold: true, align: align}
		default:
			return cell{}
		}
	}

	t := r.thread
	switch col {
	case 0:
		return cell{text: truncate(t.Identity(), columns[col].chars), muted: !t.Identified(), align: align}
	case 1:
		return cell{text: fmt.Sprintf("%d", t.TID), muted: true, align: align}
	case 2:
		style := styleForRole(t.Role)
		return cell{text: style.icon + "  " + t.Role, colour: style.colour, align: align}
	case 3:
		return cell{text: formatCycles(t.CyclesRate), muted: true, align: align}
	case 4:
		return cell{text: fmt.Sprintf("%.0f", t.SwitchRate), muted: true, align: align}
	case 5:
		return cell{text: fmt.Sprintf("%d", t.Priority), muted: true, align: align}
	case 6:
		text := t.Bucket
		if !t.Stable {
			text += " ~" // still inside the hysteresis window
		}
		if t.Starved {
			text += " ⚠"
		}
		return cell{text: text, colour: colourForBucket(t.Bucket), align: align}
	case 7:
		if t.Applied == "" {
			return cell{text: "—", muted: true, align: align}
		}
		return cell{text: truncate(t.Applied, columns[col].chars), colour: colourGood, align: align}
	default:
		return cell{}
	}
}

// formatCycles keeps the column narrow and scannable: a game's hot thread is
// billions of cycles a second and its pool threads are zero, and printing both
// in the same unit wastes the width on leading zeroes.
func formatCycles(rate float64) string {
	switch {
	case rate >= 1e9:
		return fmt.Sprintf("%.2fG", rate/1e9)
	case rate >= 1e6:
		return fmt.Sprintf("%.1fM", rate/1e6)
	case rate > 0:
		return fmt.Sprintf("%.0fK", rate/1e3)
	default:
		return "—"
	}
}

// titleText is the headline line: game, PID, phase, and the governor's mode.
func titleText(view governor.ViewModel) string {
	if view.GameName == "" {
		return "ThreadOrchestra"
	}
	return fmt.Sprintf("%s   ·   pid %d", view.GameName, view.PID)
}

// statusText is the quieter second line: what the governor is doing and what
// it is measuring.
func statusText(view governor.ViewModel) string {
	if view.GameName == "" {
		return "waiting for the first sample…"
	}
	focus := "focused"
	if !view.Focused {
		focus = "unfocused — metrics paused"
	}
	return fmt.Sprintf("%s  ·  %s / %s  ·  %s mode  ·  %s\n%d threads  ·  %s cyc/s  ·  %.1f MB/s read",
		view.Phase, view.Optimisation, view.Aggression, view.AccessMode, focus,
		view.ThreadCount, formatCycles(view.TotalCycles), view.ReadRate/1e6)
}

// chip is one coloured count in the bucket summary.
type chip struct {
	text   string
	colour color.NRGBA
}

// bucketSummary counts how each thread was classified — an at-a-glance picture
// of what the governor is doing without reading the table. Returned as
// per-bucket chips so each can carry its bucket's colour.
func bucketSummary(view governor.ViewModel) []chip {
	if view.GameName == "" {
		return nil
	}

	counts := map[string]int{}
	for _, r := range view.Rows {
		bucket := r.Bucket
		if _, known := bucketOrderIndex[bucket]; !known {
			bucket = "unclassified"
		}
		counts[bucket]++
	}

	chips := make([]chip, 0, len(bucketOrder))
	for _, bucket := range bucketOrder {
		chips = append(chips, chip{
			text:   fmt.Sprintf("%s %d", bucket, counts[bucket]),
			colour: colourForBucket(bucket),
		})
	}
	return chips
}

var bucketOrder = []string{"critical", "interactive", "background", "untouchable", "unclassified"}

var bucketOrderIndex = func() map[string]int {
	index := make(map[string]int, len(bucketOrder))
	for i, bucket := range bucketOrder {
		index[bucket] = i
	}
	return index
}()

// warningLines returns the capability/privilege warnings, one display line
// each. They are separate lines rather than one joined string because the
// warning strip is drawn as canvas text, which has no concept of a newline.
func warningLines(view governor.ViewModel) []string {
	lines := make([]string, 0, len(view.Warnings))
	for _, warning := range view.Warnings {
		lines = append(lines, "⚠  "+warning)
	}
	return lines
}

// hiddenText describes how many threads the notability filter is collapsing.
func hiddenText(view governor.ViewModel, shown int) string {
	hidden := len(view.Rows) - shown
	if hidden <= 0 {
		return fmt.Sprintf("%d threads", len(view.Rows))
	}
	return fmt.Sprintf("%d of %d shown  ·  %d idle hidden", shown, len(view.Rows), hidden)
}

// countThreads counts the thread rows in a display list, ignoring the group
// headers that were interleaved into it.
func countThreads(rows []displayRow) int {
	n := 0
	for _, r := range rows {
		if !r.header {
			n++
		}
	}
	return n
}

func truncate(s string, max int) string {
	if max <= 1 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
