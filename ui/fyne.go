//go:build windows && amd64 && !nogui

package ui

import (
	"sync/atomic"
	"time"

	"ThreadOrchestra/governor"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

// appID uniquely identifies the app to Fyne's preferences store.
const appID = "com.threadorchestra.governor"

func init() { runner = fyneRun }

// uiModel is the render state. It is only ever touched on the Fyne loop
// goroutine (inside fyne.Do or a widget callback), so it needs no lock; the
// governor hands work across the goroutine boundary through an atomic pointer.
type uiModel struct {
	view    governor.ViewModel
	showAll bool
	visible []governor.ThreadRow
}

// fyneRun builds the window, wires the governor's view-model stream into a live
// dashboard, and blocks on the Fyne event loop until the window is closed.
func fyneRun(g *governor.Governor) error {
	// Metadata opts this app into the fyne.Do threading model (silences the
	// migration banner) and gives it a stable ID for the preferences store.
	app.SetMetadata(fyne.AppMetadata{
		ID:         appID,
		Name:       "ThreadOrchestra",
		Migrations: map[string]bool{"fyneDo": true},
	})
	application := app.NewWithID(appID)
	window := application.NewWindow("ThreadOrchestra")
	window.Resize(fyne.NewSize(1100, 720))

	model := &uiModel{}

	// Header: a bold headline, a muted metrics line, the bucket summary, and a
	// warnings line that hides itself when there's nothing to say.
	title := widget.NewLabelWithStyle(titleText(governor.ViewModel{}), fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	metrics := widget.NewLabel("")
	summary := widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Italic: true})
	warnings := widget.NewLabel("")
	warnings.Hide()

	header := container.NewVBox(title, metrics, summary, warnings, widget.NewSeparator())

	// Focused thread table. Length and cells read model.visible, which is only
	// rebuilt on this same goroutine, so reads are always consistent.
	table := widget.NewTable(
		func() (int, int) { return len(model.visible), len(columns) },
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(id widget.TableCellID, cell fyne.CanvasObject) {
			cell.(*widget.Label).SetText(cellText(model.visible, id.Row, id.Col))
		},
	)
	table.ShowHeaderRow = true
	table.CreateHeader = func() fyne.CanvasObject { return widget.NewLabel("") }
	table.UpdateHeader = func(id widget.TableCellID, cell fyne.CanvasObject) {
		cell.(*widget.Label).SetText(columns[id.Col].title)
	}
	for i, col := range columns {
		table.SetColumnWidth(i, col.width)
	}

	// refresh rebuilds render state from the latest published view and repaints.
	// It must run on the loop goroutine (directly from a widget callback, or via
	// fyne.Do from the ticker).
	var latest atomic.Pointer[governor.ViewModel]
	refresh := func() {
		if v := latest.Load(); v != nil {
			model.view = *v
		}
		model.visible = visibleRows(model.view.Rows, model.showAll)

		title.SetText(titleText(model.view))
		metrics.SetText(metricsText(model.view))
		summary.SetText(bucketSummary(model.view))
		if w := warningsText(model.view); w != "" {
			warnings.SetText(w)
			warnings.Show()
		} else {
			warnings.Hide()
		}
		table.Refresh()
	}

	// Controls: pause/resume tuning, revert everything, a live hidden-thread
	// count, and a toggle to expand past the notability filter.
	pause := widget.NewButton("Pause tuning", nil)
	pause.OnTapped = func() {
		if g.TogglePause() {
			pause.SetText("Resume tuning")
		} else {
			pause.SetText("Pause tuning")
		}
	}
	revert := widget.NewButton("Revert all", g.RevertAll)

	hiddenLabel := widget.NewLabel("")
	showAll := widget.NewCheck("Show all threads", func(on bool) {
		model.showAll = on
		refresh()
		hiddenLabel.SetText(hiddenText(model.view, len(model.visible)))
	})

	controls := container.NewBorder(
		widget.NewSeparator(), nil,
		container.NewHBox(pause, revert),
		container.NewHBox(hiddenLabel, showAll),
	)

	// hiddenLabel tracks the filter; keep it in sync on every repaint too.
	repaint := func() {
		refresh()
		hiddenLabel.SetText(hiddenText(model.view, len(model.visible)))
	}

	window.SetContent(container.NewBorder(header, controls, nil, nil, table))

	// One goroutine keeps the atomic pointer pointed at the newest view; the
	// repaint ticker pulls it onto the loop goroutine at a fixed ~3 Hz so a fast
	// poll interval never floods the UI.
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case view := <-g.Views:
				v := view
				latest.Store(&v)
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(333 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fyne.Do(repaint)
			}
		}
	}()

	window.SetOnClosed(func() { close(done) })
	window.ShowAndRun()
	return nil
}
