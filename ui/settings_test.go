//go:build windows && amd64 && !nogui

package ui

import (
	"testing"

	"ThreadOrchestra/config"
	"ThreadOrchestra/governor"

	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

// The settings panel is three hundred lines of widget wiring between a config
// struct and a running governor, and every one of its failure modes is silent:
// an editor bound to the wrong field, a reset that does not refresh, an Apply
// that hands over a stale draft. Driving it headlessly is the only way to know
// the wiring is right without launching a game.

func testSession(t *testing.T) *governor.Governor {
	t.Helper()
	test.NewApp()

	auto := config.DefaultAuto()
	auto.Aggression = config.AggressionAggressive
	auto.Tuning = config.DefaultTuning(config.AggressionAggressive)

	// pid 0 never resolves, so nothing is opened and no thread is touched. The
	// panel only reads the tuning table and the game name.
	return governor.New("test.exe", config.Game{Auto: &auto}, 0)
}

func row(t *testing.T, p *settingsPanel, path string) *settingRow {
	t.Helper()
	for _, r := range p.rows {
		if r.setting.Path == path {
			return r
		}
	}
	t.Fatalf("no row for %q", path)

	return nil
}

func TestPanelBuildsARowForEverySetting(t *testing.T) {
	p := newSettingsPanel()
	p.attach(testSession(t))

	if len(p.rows) != len(p.settings) {
		t.Fatalf("built %d rows for %d settings", len(p.rows), len(p.settings))
	}
	if len(p.rows) == 0 {
		t.Fatal("no rows at all")
	}
	if !p.save.Disabled() == false {
		t.Error("save should be enabled once a session is attached")
	}
}

// The change the user asked for, driven end to end: lower the interactive
// bucket instead of raising it, and confirm the governor is running with it.
func TestEditingABucketReachesTheGovernor(t *testing.T) {
	g := testSession(t)
	p := newSettingsPanel()
	p.attach(g)

	mode := row(t, p, "buckets.interactive.priority_mode")
	priority := row(t, p, "buckets.interactive.priority")

	mode.record(p, config.PriorityLower)
	priority.record(p, "-1")

	// The draft holds the edit; the live table must not, until it is applied.
	live, generation := g.Tuning()
	if live.Buckets.Interactive.PriorityMode != config.PriorityRaise {
		t.Error("an unapplied edit reached the running session")
	}

	p.commit(false)

	live, applied := g.Tuning()
	if live.Buckets.Interactive.PriorityMode != config.PriorityLower {
		t.Errorf("priority_mode = %q, want %q", live.Buckets.Interactive.PriorityMode, config.PriorityLower)
	}
	if live.Buckets.Interactive.Priority != -1 {
		t.Errorf("priority = %d, want -1", live.Buckets.Interactive.Priority)
	}
	if applied == generation {
		t.Error("the tuning revision must move, or the actuator will not re-tune")
	}
}

func TestARefusedValueNeverReachesTheDraft(t *testing.T) {
	g := testSession(t)
	p := newSettingsPanel()
	p.attach(g)

	ready := row(t, p, "gates.starvation_ready_ratio")
	ready.record(p, "0.1")
	ready.record(p, "17")

	if ready.error.Hidden {
		t.Error("a refused value must say why")
	}
	if p.draft.Gates.StarvationReadyRatio != 0.1 {
		t.Errorf("draft = %v, want the last good value 0.1", p.draft.Gates.StarvationReadyRatio)
	}

	// And a good value afterwards clears the complaint.
	ready.record(p, "0.15")
	if !ready.error.Hidden {
		t.Error("the error must clear once the value is usable")
	}
}

// Per-setting reset is the control the user asked for; it has to put the widget
// back too, not just the struct behind it.
func TestResetPutsBothTheValueAndTheWidgetBack(t *testing.T) {
	g := testSession(t)
	p := newSettingsPanel()
	p.attach(g)

	windows := row(t, p, "gates.stable_windows")
	if !windows.reset.Disabled() {
		t.Error("an untouched setting needs no reset control")
	}

	windows.record(p, "11")
	if windows.reset.Disabled() {
		t.Error("a changed setting must offer a reset")
	}

	windows.reset.OnTapped()
	if p.draft.Gates.StableWindows != 3 {
		t.Errorf("stable_windows = %d, want the preset 3", p.draft.Gates.StableWindows)
	}
	if !windows.reset.Disabled() {
		t.Error("after a reset the control must go quiet again")
	}
}

func TestResetAllRestoresEveryEditedSetting(t *testing.T) {
	g := testSession(t)
	p := newSettingsPanel()
	p.attach(g)

	row(t, p, "gates.cooldown_ms").record(p, "1000")
	row(t, p, "buckets.critical.priority").record(p, "1")
	row(t, p, "roles.audio.eco_qos").record(p, "true")

	p.resetAll()

	defaults := config.DefaultTuning(config.AggressionAggressive)
	if p.draft.Gates.CooldownMS != defaults.Gates.CooldownMS ||
		p.draft.Buckets.Critical.Priority != defaults.Buckets.Critical.Priority ||
		p.draft.Roles.Audio.EcoQoS != nil {
		t.Errorf("reset all left edits behind: %+v", p.draft.Gates)
	}
	for _, r := range p.rows {
		if !r.reset.Disabled() {
			t.Errorf("%s still reports as changed after reset all", r.setting.Path)
		}
	}
}

// Saving with no config hook must still apply, and must say what happened
// rather than failing silently.
func TestSaveWithoutAConfigFileStillApplies(t *testing.T) {
	g := testSession(t)
	p := newSettingsPanel()
	p.attach(g)

	row(t, p, "gates.cooldown_ms").record(p, "5000")
	p.commit(true)

	if live, _ := g.Tuning(); live.Gates.CooldownMS != 5000 {
		t.Errorf("cooldown = %d, want the applied 5000", live.Gates.CooldownMS)
	}
	if p.status.Hidden || p.status.Text == "" {
		t.Fatal("a failed save must be reported")
	}
}

func TestDetachLeavesNothingEditable(t *testing.T) {
	p := newSettingsPanel()
	p.attach(testSession(t))
	p.detach()

	if p.draft != nil || len(p.rows) != 0 {
		t.Error("detach must drop the draft with the session")
	}
	if !p.save.Disabled() || !p.apply.Disabled() || !p.reset.Disabled() {
		t.Error("nothing should be actionable without a session")
	}

	// resetAll and commit are reachable from a stale click; neither may panic.
	p.resetAll()
	p.commit(true)
}

// A regression guard for the editor/registry pairing: a bool must get a check
// box, a closed set a dropdown, and a number a text field.
func TestEditorKindsMatchTheRegistry(t *testing.T) {
	p := newSettingsPanel()
	p.attach(testSession(t))

	if _, ok := editorFor(t, p, "buckets.critical.ideal_core").(*widget.Check); !ok {
		t.Error("a plain bool wants a check box")
	}
	if _, ok := editorFor(t, p, "buckets.critical.priority_mode").(*widget.Select); !ok {
		t.Error("a closed set wants a dropdown")
	}
	if _, ok := editorFor(t, p, "gates.demote_roles").(*widget.Entry); !ok {
		t.Error("a list wants a text field")
	}
}

// editorFor rebuilds a row's editor to inspect its type. The panel does not
// retain the widget, and it does not need to — sync closes over it.
func editorFor(t *testing.T, p *settingsPanel, path string) any {
	t.Helper()

	return p.buildEditor(newSettingRow(row(t, p, path).setting))
}

// The dashboard grew a tab bar, and attach/detach now drive two panels rather
// than one. Both transitions run on every game launch and exit, so a nil left
// behind by either is a crash the user meets first.
func TestDashboardAttachesAndDetachesBothTabs(t *testing.T) {
	g := testSession(t)
	d := newDashboard()

	if d.tabs == nil || len(d.tabs.Items) != 2 {
		t.Fatalf("want a Threads and a Settings tab, got %v", d.tabs)
	}

	d.attach(g)
	if d.session.Load() != g {
		t.Error("the dashboard did not take the session")
	}
	if len(d.settings.rows) == 0 {
		t.Error("the settings tab was not populated on attach")
	}
	if !d.showingThreads() {
		t.Error("a new session should open on the threads tab")
	}

	d.repaint(nil)

	// The table must not be repainted while the settings tab is up, but the
	// header above both tabs still has to update.
	d.tabs.SelectIndex(1)
	if d.showingThreads() {
		t.Error("selecting the settings tab should stop the table repainting")
	}
	d.repaint(nil)

	d.detach()
	if d.session.Load() != nil || len(d.settings.rows) != 0 {
		t.Error("detach left the session or its settings behind")
	}
}
