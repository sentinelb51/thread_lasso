package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// find is the lookup the tests use; a missing path is a schema bug, not a test
// bug, so it fails loudly.
func find(t *testing.T, settings []Setting, path string) *Setting {
	t.Helper()
	for i := range settings {
		if settings[i].Path == path {
			return &settings[i]
		}
	}
	t.Fatalf("no setting at %q", path)

	return nil
}

func registry(t *testing.T) (*Tuning, []Setting) {
	t.Helper()
	live := DefaultTuning(AggressionAggressive)
	defaults := DefaultTuning(AggressionAggressive)

	return &live, Settings(&live, &defaults)
}

// Every knob must be reachable by the UI and the reference dump, and must carry
// the sentence that explains it — an undescribed setting is a setting nobody
// can safely change, which defeats the point of exposing it.
func TestEverySettingIsDescribedAndGrouped(t *testing.T) {
	_, settings := registry(t)

	if len(settings) < 60 {
		t.Fatalf("registry walked only %d settings; the schema should expose far more", len(settings))
	}

	seen := make(map[string]bool, len(settings))
	for i := range settings {
		setting := &settings[i]
		switch {
		case setting.Desc == "":
			t.Errorf("%s: no desc tag", setting.Path)
		case setting.Group == "":
			t.Errorf("%s: no group", setting.Path)
		case setting.Label == "":
			t.Errorf("%s: no label", setting.Path)
		case seen[setting.Path]:
			t.Errorf("%s: duplicate path", setting.Path)
		}
		seen[setting.Path] = true
	}
}

func TestSettingsReachNestedAndOptionalFields(t *testing.T) {
	_, settings := registry(t)

	bucket := find(t, settings, "buckets.interactive.priority_mode")
	if bucket.Kind != KindChoice || bucket.String() != PriorityRaise {
		t.Errorf("interactive priority_mode = %q (kind %v), want %q", bucket.String(), bucket.Kind, PriorityRaise)
	}

	// The aggressive preset carves telemetry out of its bucket's I/O policy;
	// that carve-out has to be visible as an ordinary setting.
	override := find(t, settings, "roles.telemetry.io_priority")
	if !override.Optional {
		t.Error("a role override must be optional")
	}
	if override.String() != IoLow {
		t.Errorf("telemetry io_priority = %q, want %q", override.String(), IoLow)
	}
	if override.Choices[0] != Inherit {
		t.Errorf("an optional choice must offer %q first, got %v", Inherit, override.Choices)
	}

	// A role with no override reads as inheriting, and inherits in fact.
	network := find(t, settings, "roles.network/voice.io_priority")
	if network.String() != Inherit {
		t.Errorf("network io_priority = %q, want %q", network.String(), Inherit)
	}
}

func TestSetRejectsValuesOutOfRangeAndLeavesTheOldOne(t *testing.T) {
	live, settings := registry(t)
	ready := find(t, settings, "gates.starvation_ready_ratio")

	if err := ready.Set("0.15"); err != nil {
		t.Fatalf("Set(0.15): %v", err)
	}
	if live.Gates.StarvationReadyRatio != 0.15 {
		t.Errorf("StarvationReadyRatio = %v, want 0.15", live.Gates.StarvationReadyRatio)
	}

	if err := ready.Set("4"); err == nil {
		t.Error("Set(4) on a 0..1 ratio should be refused")
	}
	if live.Gates.StarvationReadyRatio != 0.15 {
		t.Errorf("a refused Set changed the value to %v", live.Gates.StarvationReadyRatio)
	}

	if err := ready.Set("banana"); err == nil {
		t.Error("Set(banana) should be refused")
	}
}

func TestResetRestoresThePresetNotTheZeroValue(t *testing.T) {
	live, settings := registry(t)
	priority := find(t, settings, "buckets.critical.priority")

	if err := priority.Set("1"); err != nil {
		t.Fatalf("Set(1): %v", err)
	}
	if priority.IsDefault() {
		t.Error("a changed setting must not report as default")
	}

	priority.Reset()
	if live.Buckets.Critical.Priority != 2 {
		t.Errorf("after Reset, critical priority = %d, want the preset 2", live.Buckets.Critical.Priority)
	}
	if !priority.IsDefault() {
		t.Error("a reset setting must report as default")
	}
}

func TestOptionalSettingsRoundTripThroughInherit(t *testing.T) {
	live, settings := registry(t)
	eco := find(t, settings, "roles.loader.eco_qos")

	if eco.String() != Inherit {
		t.Fatalf("loader eco_qos = %q, want %q", eco.String(), Inherit)
	}
	if err := eco.Set("false"); err != nil {
		t.Fatalf("Set(false): %v", err)
	}
	if live.Roles.Loader.EcoQoS == nil || *live.Roles.Loader.EcoQoS {
		t.Error("an explicit false override must be stored, not treated as unset")
	}

	if err := eco.Set(Inherit); err != nil {
		t.Fatalf("Set(inherit): %v", err)
	}
	if live.Roles.Loader.EcoQoS != nil {
		t.Error("setting inherit must clear the override")
	}
}

// Reset must not alias the defaults, or editing one setting would silently
// rewrite the table it is meant to be compared against.
func TestResetCopiesInsteadOfAliasing(t *testing.T) {
	live := DefaultTuning(AggressionStandard)
	defaults := DefaultTuning(AggressionStandard)
	settings := Settings(&live, &defaults)

	roles := find(t, settings, "gates.demote_roles")
	if err := roles.Set("telemetry"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	roles.Reset()

	live.Gates.DemoteRoles[0] = "clobbered"
	if defaults.Gates.DemoteRoles[0] == "clobbered" {
		t.Error("Reset aliased the defaults slice")
	}
}

func TestValidateClampsAndReportsRatherThanSilentlyAccepting(t *testing.T) {
	tuning := DefaultTuning(AggressionStandard)
	tuning.Gates.StarvationReadyRatio = 9
	tuning.Buckets.Background.PriorityMode = "sideways"

	problems := tuning.Validate(AggressionStandard)
	if len(problems) != 2 {
		t.Fatalf("Validate reported %d problems, want 2: %v", len(problems), problems)
	}
	if tuning.Gates.StarvationReadyRatio != 0.25 {
		t.Errorf("out-of-range ratio was not reset, = %v", tuning.Gates.StarvationReadyRatio)
	}
	if tuning.Buckets.Background.PriorityMode != PriorityLower {
		t.Errorf("unknown choice was not reset, = %q", tuning.Buckets.Background.PriorityMode)
	}
	for _, problem := range problems {
		if !strings.Contains(problem, "using ") {
			t.Errorf("a problem must say what was used instead: %q", problem)
		}
	}
}

// The presets are a compatibility contract: each aggression level must keep
// doing what it did when it was a switch statement in the actuator.
func TestPresetsMatchTheBehaviourTheyReplaced(t *testing.T) {
	conservative := DefaultTuning(AggressionConservative)
	if conservative.Buckets.Background.PriorityMode != PriorityOff {
		t.Error("conservative must never lower a priority")
	}
	if len(conservative.Gates.DemoteRoles) != 0 {
		t.Error("conservative must have nothing it is willing to demote")
	}

	standard := DefaultTuning(AggressionStandard)
	if standard.Buckets.Background.MemoryPriority != 0 || standard.Buckets.Background.CPUSets != CpuSetsOff {
		t.Error("standard demotes priority only, not memory or cpu sets")
	}
	for _, role := range []string{"pool-idle", "telemetry"} {
		if !standard.Gates.Demotable(role) {
			t.Errorf("standard must demote %s", role)
		}
	}
	if standard.Gates.Demotable("network/voice") {
		t.Error("standard must not demote network threads")
	}

	aggressive := DefaultTuning(AggressionAggressive)
	if aggressive.Buckets.Critical.IOPriority != IoHigh {
		t.Error("aggressive raises critical I/O priority")
	}
	if aggressive.Buckets.Background.MemoryPriority != 3 {
		t.Error("aggressive lowers background memory priority")
	}
	if !aggressive.Gates.Demotable("network/voice") {
		t.Error("aggressive enacts the whole table")
	}
}

// Loaders and socket pumps must not be throttled just because they share a
// bucket with telemetry; the override is how that survives the move to data.
func TestOnlyTelemetryIsIoThrottled(t *testing.T) {
	tuning := DefaultTuning(AggressionAggressive)

	telemetry, ok := tuning.ActionFor("background", "telemetry")
	if !ok || telemetry.IOPriority != IoLow || !telemetry.EcoQoS {
		t.Errorf("telemetry action = %+v, want low I/O and EcoQoS", telemetry)
	}

	for _, role := range []string{"network/voice", "loader", "pool-idle"} {
		action, ok := tuning.ActionFor("background", role)
		if !ok {
			t.Fatalf("no action for %s", role)
		}
		if action.IOPriority != IoOff || action.EcoQoS {
			t.Errorf("%s action = %+v, want no I/O throttling", role, action)
		}
		if action.MemoryPriority != 3 {
			t.Errorf("%s should still inherit the bucket's memory priority, got %d", role, action.MemoryPriority)
		}
	}
}

func TestLowersCoversEveryWayAThreadCanBeHeldBack(t *testing.T) {
	tests := []struct {
		name   string
		action BucketAction
		want   bool
	}{
		{"raising priority", BucketAction{Priority: 2, PriorityMode: PriorityRaise}, false},
		{"a normal-priority floor", BucketAction{Priority: 0, PriorityMode: PriorityRaise}, false},
		{"lowering priority", BucketAction{Priority: -1, PriorityMode: PriorityLower}, true},
		{"setting a negative priority", BucketAction{Priority: -2, PriorityMode: PrioritySet}, true},
		{"setting a positive priority", BucketAction{Priority: 2, PriorityMode: PrioritySet}, false},
		{"memory priority alone", BucketAction{PriorityMode: PriorityOff, MemoryPriority: 3}, true},
		{"I/O throttling alone", BucketAction{PriorityMode: PriorityOff, IOPriority: IoLow}, true},
		{"raising I/O priority", BucketAction{PriorityMode: PriorityOff, IOPriority: IoHigh}, false},
		{"EcoQoS alone", BucketAction{PriorityMode: PriorityOff, EcoQoS: true}, true},
		{"the background core set", BucketAction{PriorityMode: PriorityOff, CPUSets: CpuSetsBackground}, true},
		{"the critical core set", BucketAction{PriorityMode: PriorityOff, CPUSets: CpuSetsCritical}, false},
		{"nothing at all", BucketAction{PriorityMode: PriorityOff}, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.action.Lowers(); got != test.want {
				t.Errorf("Lowers() = %v, want %v", got, test.want)
			}
		})
	}
}

// An absent key must keep its default while an explicit zero must survive.
// Everything about the schema rests on being able to tell those apart.
func TestUnmarshalLayersOverThePresetInsteadOfZeroing(t *testing.T) {
	var auto Auto
	body := `{
	  "aggression": "aggressive",
	  "tuning": {
	    "gates": {"stable_windows": 7},
	    "buckets": {"interactive": {"priority": -1, "priority_mode": "lower"}},
	    "roles": {"audio": {"eco_qos": false}}
	  }
	}`
	if err := json.Unmarshal([]byte(body), &auto); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if problems := auto.applyDefaults(); len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}

	if auto.Tuning.Gates.StableWindows != 7 {
		t.Errorf("stable_windows = %d, want the configured 7", auto.Tuning.Gates.StableWindows)
	}
	if auto.Tuning.Gates.CooldownMS != 30000 {
		t.Errorf("an untouched key lost its default: cooldown = %d", auto.Tuning.Gates.CooldownMS)
	}
	if auto.Tuning.Buckets.Interactive.Priority != -1 ||
		auto.Tuning.Buckets.Interactive.PriorityMode != PriorityLower {
		t.Errorf("interactive = %+v, want a lowered priority", auto.Tuning.Buckets.Interactive)
	}
	if auto.Tuning.Buckets.Critical.IOPriority != IoHigh {
		t.Error("the aggressive preset must still apply to keys the file did not mention")
	}
	if auto.Tuning.Roles.Audio.EcoQoS == nil || *auto.Tuning.Roles.Audio.EcoQoS {
		t.Error("an explicit false override was lost")
	}
}

func TestLegacyKeysMigrateIntoTheTuningTable(t *testing.T) {
	var auto Auto
	body := `{"aggression":"standard","promotion_ceiling":1,"demotion_floor":-2,"stable_windows":9}`
	if err := json.Unmarshal([]byte(body), &auto); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	problems := auto.applyDefaults()
	if len(problems) != 3 {
		t.Fatalf("want a note per migrated key, got %d: %v", len(problems), problems)
	}

	if auto.Tuning.Buckets.Critical.Priority != 1 {
		t.Errorf("promotion_ceiling did not reach buckets.critical.priority: %d", auto.Tuning.Buckets.Critical.Priority)
	}
	if auto.Tuning.Buckets.Background.Priority != -2 {
		t.Errorf("demotion_floor did not reach buckets.background.priority: %d", auto.Tuning.Buckets.Background.Priority)
	}
	if auto.Tuning.Gates.StableWindows != 9 {
		t.Errorf("stable_windows did not migrate: %d", auto.Tuning.Gates.StableWindows)
	}

	// Cleared, so the next save writes the file in one form only.
	if auto.PromotionCeiling != nil || auto.DemotionFloor != nil || auto.StableWindows != 0 {
		t.Error("migrated keys must be cleared")
	}
	encoded, err := json.Marshal(auto)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(encoded), "promotion_ceiling") {
		t.Error("a migrated key was written back out")
	}
}

func TestDefaultAutoIsFullyPopulated(t *testing.T) {
	auto := DefaultAuto()
	if auto.Tuning.Gates.PollIntervalMS == 0 || auto.Tuning.Signals.MinScore == 0 {
		t.Fatal("a Go-constructed Auto must still get the tuning defaults")
	}
	if auto.Optimisation != "observe" {
		t.Errorf("optimisation = %q, want the safe default", auto.Optimisation)
	}
}

func TestLabelsReadAsEnglish(t *testing.T) {
	_, settings := registry(t)

	want := map[string]string{
		"gates.poll_interval_ms":       "Poll interval (ms)",
		"signals.cadence_cv":           "Cadence variation",
		"buckets.critical.io_priority": "I/O priority",
		"buckets.critical.eco_qos":     "Eco QoS",
		"scan.stack_interval_s":        "Stack interval (s)",
		"signals.hot_share_lo":         "Hot share lower bound",
	}
	for path, label := range want {
		if got := find(t, settings, path).Label; got != label {
			t.Errorf("%s label = %q, want %q", path, got, label)
		}
	}
}
