//go:build windows && amd64 && !nogui

package ui

import (
	"fmt"
	"strings"
	"sync/atomic"

	"ThreadOrchestra/config"
	"ThreadOrchestra/governor"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// The settings panel edits a draft copy of the session's tuning table. Nothing
// reaches the running governor until Apply, and nothing reaches config.json
// until Save, so a half-typed number is never something the machine acts on.

// labelWidth keeps the setting names in a column. Descriptions sit underneath
// at full width rather than beside, because they are sentences, not captions.
const labelWidth = 260

// settingsPanel is the Settings tab: every knob in the tuning table, grouped,
// described, and individually resettable.
type settingsPanel struct {
	content *fyne.Container
	body    *fyne.Container
	status  *widget.Label

	session atomic.Pointer[governor.Governor]

	// draft is the edited copy; settings are bound to it, and rows are the
	// widgets showing them. All three are replaced together on attach.
	draft    *config.Tuning
	settings []config.Setting
	rows     []*settingRow

	apply  *widget.Button
	save   *widget.Button
	reset  *widget.Button
	footer *fyne.Container
}

// settingRow is one knob's widgets plus the two callbacks that keep them in
// step with the draft: sync pushes the draft's value into the editor, and the
// editor's own handler pushes edits back.
type settingRow struct {
	setting *config.Setting
	error   *widget.Label
	reset   *widget.Button
	sync    func()

	// quiet suppresses the editor's change handler while sync is writing into
	// it. Fyne's setters fire OnChanged whoever called them, so without this a
	// programmatic refresh is indistinguishable from typing — which would clear
	// the status line the refresh was meant to explain.
	quiet bool
}

// newSettingRow builds the parts every row has, whatever kind of editor it ends
// up with.
func newSettingRow(setting *config.Setting) *settingRow {
	row := &settingRow{setting: setting}

	row.error = widget.NewLabel("")
	row.error.Wrapping = fyne.TextWrapWord
	row.error.Importance = widget.DangerImportance
	row.error.Hide()

	return row
}

// update runs a programmatic widget change without it reading as user input.
func (row *settingRow) update(change func()) {
	row.quiet = true
	change()
	row.quiet = false
}

func newSettingsPanel() *settingsPanel {
	p := &settingsPanel{}

	p.status = widget.NewLabel("")
	p.status.Wrapping = fyne.TextWrapWord

	p.apply = widget.NewButton("Apply to this session", func() { p.commit(false) })
	p.save = widget.NewButton("Apply and save to config.json", func() { p.commit(true) })
	p.save.Importance = widget.HighImportance
	p.reset = widget.NewButton("Reset all to preset", p.resetAll)

	p.footer = container.NewVBox(
		widget.NewSeparator(),
		container.NewHBox(p.save, p.apply, p.reset),
		p.status,
	)

	p.body = container.NewVBox()
	p.content = container.NewBorder(nil, p.footer, nil, nil, container.NewVScroll(p.body))
	p.detach()

	return p
}

// attach rebuilds the panel for a new session. The widget tree is thrown away
// and remade rather than repopulated: it happens once per game launch, and the
// alternative is keeping two trees in sync for no benefit.
func (p *settingsPanel) attach(g *governor.Governor) {
	p.session.Store(g)
	p.draft, p.settings = g.Draft()
	p.rows = nil

	groups := config.Groups(p.settings)
	accordion := widget.NewAccordion()
	accordion.MultiOpen = true

	for _, group := range groups {
		rows := container.NewVBox()
		for i := range p.settings {
			if p.settings[i].Group == group {
				rows.Add(p.buildRow(&p.settings[i]))
			}
		}
		accordion.Append(widget.NewAccordionItem(group, rows))
	}
	// Bucket actions are what almost everyone came for; the rest is opened on
	// demand.
	if len(groups) > 0 {
		accordion.Open(0)
	}

	p.body.RemoveAll()
	p.body.Add(widget.NewLabelWithStyle(
		fmt.Sprintf("Tuning for %s — defaults are the %q preset.", g.GameName(), g.Aggression()),
		fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
	p.body.Add(widget.NewLabel(
		"Changes apply to the running session and re-tune every thread the governor has touched. " +
			"Saving writes them into config.json for next time."))
	p.body.Add(accordion)
	p.body.Refresh()

	p.setStatus("")
	p.enable(true)
	p.refreshRows()
}

// detach empties the panel when no game is attached. There is nothing to edit
// without a session: the tuning table belongs to the game, not to the app.
func (p *settingsPanel) detach() {
	p.session.Store(nil)
	p.draft, p.settings, p.rows = nil, nil, nil

	p.body.RemoveAll()
	p.body.Add(widget.NewLabel("Settings become editable once a game is attached."))
	p.body.Refresh()

	p.setStatus("")
	p.enable(false)
}

func (p *settingsPanel) enable(on bool) {
	for _, button := range []*widget.Button{p.apply, p.save, p.reset} {
		if on {
			button.Enable()
		} else {
			button.Disable()
		}
	}
}

// buildRow lays out one setting: name and editor on a line, the sentence that
// explains it underneath, and a validation message that appears only when the
// value typed is unusable.
func (p *settingsPanel) buildRow(setting *config.Setting) fyne.CanvasObject {
	row := newSettingRow(setting)

	row.reset = widget.NewButtonWithIcon("", theme.ContentUndoIcon(), func() {
		setting.Reset()
		row.sync()
		row.error.Hide()
		p.setStatus("")
		p.refreshRows()
	})
	row.reset.Importance = widget.LowImportance

	editor := p.buildEditor(row)

	name := widget.NewLabel(setting.Label)
	name.Wrapping = fyne.TextWrapWord

	line := container.NewBorder(nil, nil,
		container.NewGridWrap(fyne.NewSize(labelWidth, 34), name),
		row.reset,
		editor,
	)

	description := widget.NewLabel(setting.Desc)
	description.Wrapping = fyne.TextWrapWord
	description.TextStyle = fyne.TextStyle{Italic: true}

	p.rows = append(p.rows, row)

	return container.NewVBox(line, description, row.error, widget.NewSeparator())
}

// buildEditor picks the widget for a setting's kind and wires it to the draft.
// Every path routes through Setting.Set, so the UI cannot store a value the
// config file would reject.
func (p *settingsPanel) buildEditor(row *settingRow) fyne.CanvasObject {
	setting := row.setting

	// In every case the initial value is written before the handler is attached.
	// Fyne's setters fire OnChanged regardless of who called them, so wiring
	// first would make building the panel look like the user editing it.
	switch setting.Kind {
	case config.KindChoice:
		selector := widget.NewSelect(setting.Choices, nil)
		row.sync = func() { row.update(func() { selector.SetSelected(setting.String()) }) }
		row.sync()
		selector.OnChanged = func(choice string) { row.record(p, choice) }

		return selector

	case config.KindBool:
		// A plain bool has no third state, so a checkbox is honest here; the
		// optional ones were turned into three-way choices by the registry.
		check := widget.NewCheck("", nil)
		row.sync = func() { row.update(func() { check.SetChecked(setting.String() == "true") }) }
		row.sync()
		check.OnChanged = func(on bool) { row.record(p, fmt.Sprint(on)) }

		return check

	default:
		entry := widget.NewEntry()
		if setting.Kind == config.KindStrings {
			entry.SetPlaceHolder("comma-separated, empty for none")
		}
		row.sync = func() { row.update(func() { entry.SetText(setting.String()) }) }
		row.sync()
		entry.OnChanged = func(text string) { row.record(p, text) }

		return entry
	}
}

// record stores an edit, showing the reason when the value is refused. A bad
// value leaves the draft holding the last good one, so Apply can never enact
// something half-typed.
func (row *settingRow) record(p *settingsPanel, text string) {
	if row.quiet {
		return
	}

	if err := row.setting.Set(text); err != nil {
		row.error.SetText(err.Error())
		row.error.Show()
		return
	}

	row.error.Hide()
	row.reset.Enable()
	if row.setting.IsDefault() {
		row.reset.Disable()
	}
	p.setStatus("")
}

// refreshRows re-reads every editor from the draft. Called after a bulk change
// — a reset-all, or a fresh attach — where individual handlers did not run.
func (p *settingsPanel) refreshRows() {
	for _, row := range p.rows {
		row.sync()
		row.error.Hide()
		if row.setting.IsDefault() {
			row.reset.Disable()
		} else {
			row.reset.Enable()
		}
	}
}

func (p *settingsPanel) resetAll() {
	if p.draft == nil {
		return
	}
	for i := range p.settings {
		p.settings[i].Reset()
	}

	p.refreshRows()
	p.setStatus("Reset to the preset. Nothing is applied until you press Apply or Save.")
}

// commit hands the draft to the governor, optionally writing it to disk too.
func (p *settingsPanel) commit(persist bool) {
	g := p.session.Load()
	if g == nil || p.draft == nil {
		return
	}

	var problems []string
	var err error
	if persist {
		problems, err = g.SaveTuning(*p.draft)
	} else {
		problems = g.ApplyTuning(*p.draft)
	}

	// Validation may have reset a value on the way in, so the editors are
	// re-read rather than left showing what was refused.
	p.refreshRows()

	switch {
	case err != nil:
		p.setStatus("Applied to this session, but saving failed: " + err.Error())
	case len(problems) > 0:
		p.setStatus("Applied, with corrections: " + strings.Join(problems, "; "))
	case persist:
		p.setStatus("Applied and saved to " + config.Path() + ".")
	default:
		p.setStatus("Applied to this session. Not saved — it will be back to the file's values next launch.")
	}
}

func (p *settingsPanel) setStatus(text string) {
	p.status.SetText(text)
	if text == "" {
		p.status.Hide()
		return
	}
	p.status.Show()
}
