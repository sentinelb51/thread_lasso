//go:build windows && amd64 && !nogui

package ui

import (
	"strings"
	"sync/atomic"
	"time"

	"ThreadOrchestra/governor"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// appID uniquely identifies the app to Fyne's preferences store.
const appID = "com.threadorchestra.governor"

// repaintInterval decouples the repaint rate from the governor's poll
// interval, so a fast poll never floods the UI.
const repaintInterval = 333 * time.Millisecond

func init() { runner = fyneRun }

var alignments = map[textAlign]fyne.TextAlign{
	alignLeading:  fyne.TextAlignLeading,
	alignTrailing: fyne.TextAlignTrailing,
}

// fyneRun owns the window for the life of the process. It shows the waiting
// screen until the supervisor reports a game, swaps in the dashboard for the
// session, and swaps back when the game exits — the scan is part of the app
// now, not a console line behind it.
func fyneRun(feed Feed) error {
	// Metadata opts this app into the fyne.Do threading model (silences the
	// migration banner) and gives it a stable ID for the preferences store.
	app.SetMetadata(fyne.AppMetadata{
		ID:         appID,
		Name:       "ThreadOrchestra",
		Migrations: map[string]bool{"fyneDo": true},
	})
	application := app.NewWithID(appID)
	window := application.NewWindow("ThreadOrchestra")
	window.Resize(fyne.NewSize(1120, 760))

	waiting := newWaitScreen(feed.Watching)
	dash := newDashboard()

	// One stack, both screens, visibility as the switch: rebuilding the content
	// tree on every transition would drop the table's scroll position and the
	// "show all" toggle every time a game restarted.
	screens := container.NewStack(waiting.content, dash.content)
	dash.content.Hide()
	window.SetContent(screens)

	var latest atomic.Pointer[governor.ViewModel]

	showWaiting := func(status string, fatal bool) {
		dash.detach()
		latest.Store(nil)
		waiting.set(status, fatal)
		dash.content.Hide()
		waiting.content.Show()
	}
	showDashboard := func(g *governor.Governor) {
		dash.attach(g)
		latest.Store(nil)
		waiting.content.Hide()
		dash.content.Show()
	}

	done := make(chan struct{})

	// A single goroutine owns both streams. views is nil while no session is
	// attached, and a receive on a nil channel blocks forever — which is
	// exactly the behaviour wanted, with no extra state to keep in sync.
	go func() {
		var views <-chan governor.ViewModel
		for {
			select {
			case <-done:
				return

			case event, ok := <-feed.Events:
				if !ok {
					return
				}
				if event.Session != nil {
					session := event.Session
					views = session.Views
					fyne.Do(func() { showDashboard(session) })
					continue
				}
				views = nil
				status, fatal := event.Status, event.Fatal
				fyne.Do(func() { showWaiting(status, fatal) })

			case view := <-views:
				v := view
				latest.Store(&v)
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(repaintInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fyne.Do(func() { dash.repaint(latest.Load()) })
			}
		}
	}()

	window.SetOnClosed(func() {
		close(done)
		if feed.OnQuit != nil {
			feed.OnQuit()
		}
	})
	window.ShowAndRun()
	return nil
}

// waitScreen is what the app shows when no game is running.
type waitScreen struct {
	content *fyne.Container
	status  *canvas.Text
	spinner *widget.ProgressBarInfinite
}

func newWaitScreen(watching []string) *waitScreen {
	title := canvas.NewText("ThreadOrchestra", theme.Color(theme.ColorNameForeground))
	title.TextSize = 30
	title.TextStyle = fyne.TextStyle{Bold: true}
	title.Alignment = fyne.TextAlignCenter

	subtitle := canvas.NewText("per-thread scheduling governor", theme.Color(theme.ColorNamePlaceHolder))
	subtitle.Alignment = fyne.TextAlignCenter

	status := canvas.NewText("Starting…", theme.Color(theme.ColorNameForeground))
	status.Alignment = fyne.TextAlignCenter

	spinner := widget.NewProgressBarInfinite()

	watched := "no games configured"
	if len(watching) > 0 {
		watched = "watching for   " + strings.Join(watching, "   ·   ")
	}
	list := canvas.NewText(watched, theme.Color(theme.ColorNamePlaceHolder))
	list.Alignment = fyne.TextAlignCenter

	body := container.NewVBox(
		title,
		subtitle,
		widget.NewSeparator(),
		status,
		// The spinner's own minimum width is tiny; give it something that reads
		// as a progress indicator rather than a dash.
		container.NewGridWrap(fyne.NewSize(440, 24), spinner),
		list,
	)

	return &waitScreen{
		content: container.NewCenter(body),
		status:  status,
		spinner: spinner,
	}
}

// set updates the waiting message. A fatal status means the supervisor has
// stopped, so the spinner stops implying that something is still happening.
func (w *waitScreen) set(status string, fatal bool) {
	if status != "" {
		w.status.Text = status
	}
	if fatal {
		w.status.Color = colourWarning
		w.spinner.Hide()
	} else {
		w.status.Color = theme.Color(theme.ColorNameForeground)
		w.spinner.Show()
	}
	w.status.Refresh()
}

// dashboard is the live per-session view.
type dashboard struct {
	content *fyne.Container

	// The header is shared by both tabs, so the game, phase and warnings stay
	// visible while settings are being edited — the two are read together.
	tabs     *container.AppTabs
	settings *settingsPanel

	title  *canvas.Text
	status *widget.Label
	table  *widget.Table
	hidden *widget.Label
	pause  *widget.Button

	// The bucket counts are a fixed set, so the chips are built once and only
	// their text changes — rebuilding five canvas objects three times a second
	// would relayout the header on every repaint.
	chips []*canvas.Text

	warnings     *fyne.Container
	warningCount int

	// Render state, touched only on the Fyne loop goroutine.
	view    governor.ViewModel
	rows    []displayRow
	all     bool
	session atomic.Pointer[governor.Governor]
}

func newDashboard() *dashboard {
	d := &dashboard{}

	d.title = canvas.NewText("", theme.Color(theme.ColorNameForeground))
	d.title.TextSize = 18
	d.title.TextStyle = fyne.TextStyle{Bold: true}

	// Plain weight, not italic: this line is read as often as the table, and
	// the whole header is already differentiated by size and colour.
	d.status = widget.NewLabel("")

	chipBox := container.NewHBox()
	for range bucketOrder {
		text := canvas.NewText("", theme.Color(theme.ColorNamePlaceHolder))
		text.TextStyle = fyne.TextStyle{Bold: true}
		d.chips = append(d.chips, text)
		chipBox.Add(text)
	}

	d.warnings = container.NewVBox()
	d.warnings.Hide()

	header := container.NewVBox(
		d.title,
		d.status,
		chipBox,
		d.warnings,
		widget.NewSeparator(),
	)

	d.table = widget.NewTable(
		func() (int, int) { return len(d.rows), len(columns) },
		func() fyne.CanvasObject {
			return canvas.NewText("", theme.Color(theme.ColorNameForeground))
		},
		func(id widget.TableCellID, object fyne.CanvasObject) {
			applyCell(object.(*canvas.Text), cellFor(d.rows, id.Row, id.Col))
		},
	)
	d.table.ShowHeaderRow = true
	d.table.CreateHeader = func() fyne.CanvasObject {
		text := canvas.NewText("", theme.Color(theme.ColorNamePlaceHolder))
		text.TextStyle = fyne.TextStyle{Bold: true}
		return text
	}
	d.table.UpdateHeader = func(id widget.TableCellID, object fyne.CanvasObject) {
		text := object.(*canvas.Text)
		if id.Col < 0 || id.Col >= len(columns) {
			return
		}
		text.Text = columns[id.Col].title
		text.Alignment = alignments[columns[id.Col].align]
		text.Refresh()
	}
	for i, col := range columns {
		d.table.SetColumnWidth(i, col.width)
	}

	d.pause = widget.NewButton("Pause tuning", func() {
		g := d.session.Load()
		if g == nil {
			return
		}
		if g.TogglePause() {
			d.pause.SetText("Resume tuning")
		} else {
			d.pause.SetText("Pause tuning")
		}
	})
	revert := widget.NewButton("Revert all", func() {
		if g := d.session.Load(); g != nil {
			g.RevertAll()
		}
	})

	d.hidden = widget.NewLabel("")
	showAll := widget.NewCheck("Show all threads", func(on bool) {
		d.all = on
		d.rebuild()
	})

	controls := container.NewBorder(
		widget.NewSeparator(), nil,
		container.NewHBox(d.pause, revert),
		container.NewHBox(d.hidden, showAll),
	)

	d.settings = newSettingsPanel()
	d.tabs = container.NewAppTabs(
		container.NewTabItem("Threads", container.NewBorder(nil, controls, nil, nil, d.table)),
		container.NewTabItem("Settings", d.settings.content),
	)

	d.content = container.NewBorder(header, nil, nil, nil, d.tabs)
	return d
}

// showingThreads reports whether the table is the visible tab. Repainting it
// three times a second while someone is reading the settings is work nobody can
// see.
func (d *dashboard) showingThreads() bool {
	return d.tabs == nil || d.tabs.SelectedIndex() == 0
}

// attach points the dashboard at a new session and clears the previous one's
// state, so a restarted game never shows a stale table for a frame.
func (d *dashboard) attach(g *governor.Governor) {
	d.session.Store(g)
	d.pause.SetText("Pause tuning")
	d.view = governor.ViewModel{}
	d.settings.attach(g)
	d.tabs.SelectIndex(0)
	d.rebuild()
}

func (d *dashboard) detach() {
	d.session.Store(nil)
	d.view = governor.ViewModel{}
	d.settings.detach()
	d.rebuild()
}

// repaint pulls the newest published view onto the loop goroutine and redraws.
// It is a no-op while no session is attached: the waiting screen is on top and
// there is nothing new to draw underneath it.
func (d *dashboard) repaint(view *governor.ViewModel) {
	if d.session.Load() == nil {
		return
	}
	if view != nil {
		d.view = *view
	}
	d.rebuild()
}

// rebuild regenerates the render state from d.view. It must run on the Fyne
// loop goroutine — from a widget callback or inside fyne.Do.
func (d *dashboard) rebuild() {
	if d.showingThreads() {
		d.rows = buildRows(d.view.Rows, d.all)
	}

	d.title.Text = titleText(d.view)
	d.title.Refresh()
	d.status.SetText(statusText(d.view))

	summary := bucketSummary(d.view)
	for i, text := range d.chips {
		if i < len(summary) {
			text.Text = summary[i].text
			text.Color = summary[i].colour
		} else {
			text.Text = ""
		}
		text.Refresh()
	}

	// Warnings are set once per session (plus the one-shot entry-point
	// diagnostic), so the strip is only rebuilt when the count actually moves.
	if lines := warningLines(d.view); len(lines) != d.warningCount {
		d.warningCount = len(lines)
		d.warnings.RemoveAll()
		for _, line := range lines {
			d.warnings.Add(canvas.NewText(line, colourWarning))
		}
		d.warnings.Refresh()
		if len(lines) > 0 {
			d.warnings.Show()
		} else {
			d.warnings.Hide()
		}
	}

	if d.showingThreads() {
		d.hidden.SetText(hiddenText(d.view, countThreads(d.rows)))
		d.table.Refresh()
	}
}

// applyCell paints one table cell. Colour resolution lives here rather than in
// the layout code so the Fyne-free half stays free of theme lookups.
func applyCell(text *canvas.Text, c cell) {
	text.Text = c.text
	switch {
	case c.colour != nil:
		text.Color = c.colour
	case c.muted:
		text.Color = theme.Color(theme.ColorNamePlaceHolder)
	default:
		text.Color = theme.Color(theme.ColorNameForeground)
	}
	text.TextStyle = fyne.TextStyle{Bold: c.bold}
	text.Alignment = alignments[c.align]
	text.Refresh()
}
