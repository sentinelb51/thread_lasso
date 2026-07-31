//go:build windows && amd64

package governor

import (
	"strings"
	"testing"

	"ThreadOrchestra/config"
)

// The role names are spelled in two packages: governor owns the enum, config
// owns the override table and the role_buckets keys. config cannot import
// governor without a cycle, so the agreement is asserted rather than derived —
// a role added on one side and not the other would silently stop being
// configurable.
func TestRoleNamesAgreeAcrossPackages(t *testing.T) {
	fromConfig := make(map[string]bool)
	for _, name := range config.RoleNames() {
		fromConfig[name] = true
	}

	for role := Role(1); int(role) < roleCount; role++ {
		if !fromConfig[role.String()] {
			t.Errorf("role %q has no entry in config.RoleNames; it cannot be overridden", role)
		}
	}
	if len(fromConfig) != roleCount-1 {
		t.Errorf("config knows %d roles, governor has %d", len(fromConfig), roleCount-1)
	}

	// And every config role must resolve to an action, which is the lookup the
	// actuator makes on every tick.
	tuning := config.DefaultTuning(config.AggressionAggressive)
	for _, name := range config.RoleNames() {
		if _, ok := tuning.ActionFor("background", name); !ok {
			t.Errorf("no action resolves for role %q", name)
		}
	}
}

// The bucket names are the other cross-package spelling: config keys its action
// table by them, governor prints them from an enum.
func TestBucketNamesResolveToActions(t *testing.T) {
	tuning := config.DefaultTuning(config.AggressionStandard)

	for _, bucket := range []Bucket{BucketCritical, BucketInteractive, BucketBackground} {
		if _, ok := tuning.ActionFor(bucket.String(), "job-worker"); !ok {
			t.Errorf("bucket %q has no action table", bucket)
		}
	}
	for _, bucket := range []Bucket{BucketNone, BucketUntouchable} {
		if _, ok := tuning.ActionFor(bucket.String(), "job-worker"); ok {
			t.Errorf("bucket %q must have no action — it means leave the thread alone", bucket)
		}
	}
}

// A bucket told to lower a thread does nothing unless the role is also on the
// demote list, and the two settings live in different sections. Someone who
// lowers a bucket deserves to be told when the preset's demote list makes it a
// no-op rather than discovering it by watching nothing happen.
func TestCheckPolicyReportsLoweringThatCannotHappen(t *testing.T) {
	buckets := DefaultRoleBuckets()

	// Standard demotes pool-idle and telemetry only, so lowering interactive —
	// which holds audio, job-worker and loader — is silently inert.
	preset := config.DefaultTuning(config.AggressionStandard)
	tuning := config.DefaultTuning(config.AggressionStandard)
	tuning.Buckets.Interactive.Priority = -1
	tuning.Buckets.Interactive.PriorityMode = config.PriorityLower

	problems := checkPolicy(&tuning, &preset, buckets)
	if len(problems) != 3 {
		t.Fatalf("want a warning for each of audio, job-worker and loader, got %d: %v", len(problems), problems)
	}
	for _, problem := range problems {
		if !strings.Contains(problem, "demote_roles") {
			t.Errorf("a warning must name the setting to change: %q", problem)
		}
	}

	// Adding the roles to the demote list clears every complaint.
	tuning.Gates.DemoteRoles = append(tuning.Gates.DemoteRoles, "audio", "job-worker", "loader")
	if problems := checkPolicy(&tuning, &preset, buckets); len(problems) != 0 {
		t.Errorf("unexpected warnings once the roles are demotable: %v", problems)
	}
}

// The presets contain the lowering-but-not-demotable combination on purpose —
// it is how "demote only what I am sure about" is expressed — so an unedited
// config must produce no warnings at all. A warning everyone sees is a warning
// nobody reads.
func TestCheckPolicyIsQuietOnEveryShippedPreset(t *testing.T) {
	buckets := DefaultRoleBuckets()

	for _, name := range []string{
		config.AggressionConservative,
		config.AggressionStandard,
		config.AggressionAggressive,
	} {
		preset := config.DefaultTuning(name)
		tuning := config.DefaultTuning(name)
		if problems := checkPolicy(&tuning, &preset, buckets); len(problems) != 0 {
			t.Errorf("%s preset warns about itself: %v", name, problems)
		}
	}
}

// Applying an edit has to move the revision, because that is the only signal
// the actuator has that a thread's current settings were written under rules
// that no longer exist.
func TestApplyTuningBumpsTheRevisionAndRetunesTheClassifier(t *testing.T) {
	auto := config.DefaultAuto()
	g := New("test.exe", config.Game{Auto: &auto}, 0)

	_, before := g.Tuning()

	edited := config.DefaultTuning(config.AggressionStandard)
	edited.Gates.StableWindows = 9
	if problems := g.ApplyTuning(edited); len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}

	live, after := g.Tuning()
	if after == before {
		t.Error("the revision did not move")
	}
	if live.Gates.StableWindows != 9 {
		t.Errorf("live table not swapped: stable_windows = %d", live.Gates.StableWindows)
	}
	if g.classifier.stableWindows != 9 {
		t.Errorf("classifier still on %d windows", g.classifier.stableWindows)
	}
}

// A value the config would reject must not become the live table just because
// it arrived through the API rather than the file.
func TestApplyTuningValidatesWhatItIsGiven(t *testing.T) {
	auto := config.DefaultAuto()
	g := New("test.exe", config.Game{Auto: &auto}, 0)

	edited := config.DefaultTuning(config.AggressionStandard)
	edited.Gates.StarvationReadyRatio = 50

	problems := g.ApplyTuning(edited)
	if len(problems) != 1 {
		t.Fatalf("want one complaint, got %v", problems)
	}
	if live, _ := g.Tuning(); live.Gates.StarvationReadyRatio != 0.25 {
		t.Errorf("an out-of-range ratio went live: %v", live.Gates.StarvationReadyRatio)
	}
}

// Facts built without a signals table must still score the way the governor
// scores them, or every test and the probe would be measuring something else.
func TestUnconfiguredFactsFallBackToThePreset(t *testing.T) {
	if fallbackSignals.MinScore != config.DefaultTuning(config.AggressionStandard).Signals.MinScore {
		t.Error("the fallback drifted from the standard preset")
	}

	f := &Facts{}
	if f.signals() != &fallbackSignals {
		t.Error("a nil Signals must resolve to the fallback, not to zeroes")
	}

	f.Signals = &config.Signals{MinScore: 7}
	if f.signals().MinScore != 7 {
		t.Error("a configured Signals must win")
	}
}

// A tighter min_score must make the classifier report fewer roles: it is the
// knob for "act on fewer threads with more certainty", so it has to actually
// gate the winner.
func TestMinScoreGatesTheWinner(t *testing.T) {
	series := quietSeries()
	series.Samples = 10

	f := &Facts{Series: series, Stack: []string{"dxgi.dll"}}
	if role, _ := ClassifyRole(f); role != RoleRenderSubmit {
		t.Fatalf("role = %v, want render at the default min score", role)
	}

	strict := fallbackSignals
	strict.MinScore = 20
	f.Signals = &strict
	if role, _ := ClassifyRole(f); role != RoleUnknown {
		t.Errorf("role = %v, want unknown once min_score is above any evidence", role)
	}
}
