package config

import (
	"encoding/json"
	"fmt"
)

type Config struct {
	// Readme is carried through untouched so a generated or hand-annotated
	// config keeps its notes across a save from the settings panel.
	Readme []string        `json:"_readme,omitempty"`
	Games  map[string]Game `json:"games"`
}

type Game struct {
	Priority    string   `json:"priority,omitempty"`     // "realtime", "high", "above_normal", "normal", "below_normal", "idle"
	IOPriority  string   `json:"io_priority,omitempty"`  // "high", "normal", "low", "very_low"
	GPUPriority string   `json:"gpu_priority,omitempty"` // "realtime", "high", "normal", "below_normal", "low"
	Affinity    []int    `json:"affinity,omitempty"`
	CPUSets     []int    `json:"cpu_sets,omitempty"`
	Threads     []Thread `json:"threads,omitempty"`
	Auto        *Auto    `json:"auto,omitempty"` // nil = no automatic governor
}

type Thread struct {
	Name       string `json:"name"`
	Priority   *int   `json:"priority,omitempty"`    // -15, -2, -1, 0, 1, 2, 15
	IOPriority *int   `json:"io_priority,omitempty"` // 0 - 3 = Very Low, Low, Normal, High
	Affinity   []int  `json:"affinity,omitempty"`
	CPUSets    []int  `json:"cpu_sets,omitempty"`
}

// Auto configures the automatic thread governor. The three string knobs are
// the coarse controls; everything the governor actually measures against lives
// in Tuning, where each field is described and range-checked.
type Auto struct {
	Mode         string `json:"mode,omitempty"`         // "limited", "full" — handle access level
	Optimisation string `json:"optimisation,omitempty"` // "observe", "manual", "auto"
	Aggression   string `json:"aggression,omitempty"`   // "conservative", "standard", "aggressive"

	CriticalCores   []int       `json:"critical_cores,omitempty"`
	BackgroundCores []int       `json:"background_cores,omitempty"`
	Exclude         []string    `json:"exclude,omitempty"` // name/module wildcards auto never touches
	Force           []ForceRule `json:"force,omitempty"`

	// RoleBuckets overrides which tuning bucket a classified role lands in,
	// e.g. {"network/voice": "interactive"} to keep netcode threads out of the
	// background set. Keys are role names as the UI shows them; unset roles keep
	// their default. See governor.DefaultRoleBuckets.
	RoleBuckets map[string]string `json:"role_buckets,omitempty"`

	// Tuning is always fully populated — see Tuning and UnmarshalJSON.
	Tuning Tuning `json:"tuning"`

	// Superseded keys, kept so an existing config keeps working. They are
	// migrated into Tuning on load and dropped on the next save; nothing reads
	// them after applyDefaults.
	PollIntervalMS   int  `json:"poll_interval_ms,omitempty"`
	StableWindows    int  `json:"stable_windows,omitempty"`
	CooldownMS       int  `json:"cooldown_ms,omitempty"`
	PromotionCeiling *int `json:"promotion_ceiling,omitempty"`
	DemotionFloor    *int `json:"demotion_floor,omitempty"`
}

// ForceRule pins matching threads to a bucket regardless of classification.
type ForceRule struct {
	Name   string `json:"name,omitempty"`   // thread-name wildcard
	Module string `json:"module,omitempty"` // start-module wildcard (full mode only)
	Bucket string `json:"bucket"`           // "critical", "interactive", "background", "untouchable"
}

// DefaultAuto is a governor configuration with nothing set: observe mode at
// standard aggression, with the full default tuning table. Used for a game with
// no "auto" block at all, so the rest of the code never sees a zero Tuning.
func DefaultAuto() Auto {
	auto := Auto{}
	auto.applyDefaults()

	return auto
}

// UnmarshalJSON layers the file's tuning keys over the preset for the game's
// aggression level, rather than over the zero value.
//
// This is what makes an absent key mean "keep the default" while an explicit
// zero means zero — a distinction the schema depends on, because "eco_qos":
// false and "priority": 0 are both meaningful settings that a pointer-and-
// omitempty scheme would erase.
func (a *Auto) UnmarshalJSON(data []byte) error {
	var probe struct {
		Aggression string `json:"aggression"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	if probe.Aggression == "" {
		probe.Aggression = AggressionStandard
	}

	// A distinct type so decoding the body does not recurse back into here.
	type plain Auto
	holder := plain{Tuning: DefaultTuning(probe.Aggression)}
	if err := json.Unmarshal(data, &holder); err != nil {
		return err
	}
	*a = Auto(holder)

	return nil
}

// applyDefaults fills in the coarse knobs, migrates superseded keys, and
// range-checks the tuning table. Returns one message per problem found.
func (a *Auto) applyDefaults() []string {
	if a.Mode == "" {
		a.Mode = "limited"
	}
	if a.Optimisation == "" {
		a.Optimisation = "observe"
	}
	if a.Aggression == "" {
		a.Aggression = AggressionStandard
	}

	// A zero Tuning means this Auto was built in Go rather than decoded, so
	// UnmarshalJSON never ran. Poll interval has no meaningful zero value, which
	// makes it a reliable marker.
	if a.Tuning.Gates.PollIntervalMS == 0 && a.Tuning.Signals.MinScore == 0 {
		a.Tuning = DefaultTuning(a.Aggression)
	}

	problems := a.migrate()

	return append(problems, a.Tuning.Validate(a.Aggression)...)
}

// migrate moves the pre-tuning keys into the tuning table and clears them, so
// the next save writes the file in one form only. Each one names its
// replacement, because the mapping is not always obvious: the old
// promotion_ceiling was specifically the critical bucket's priority target.
func (a *Auto) migrate() []string {
	var notes []string
	note := func(old, new string) {
		notes = append(notes, fmt.Sprintf("%q has moved to tuning.%s; migrated, and it will be written in the new form on the next save", old, new))
	}

	if a.PollIntervalMS > 0 {
		a.Tuning.Gates.PollIntervalMS = a.PollIntervalMS
		a.PollIntervalMS = 0
		note("poll_interval_ms", "gates.poll_interval_ms")
	}
	if a.StableWindows > 0 {
		a.Tuning.Gates.StableWindows = a.StableWindows
		a.StableWindows = 0
		note("stable_windows", "gates.stable_windows")
	}
	if a.CooldownMS > 0 {
		a.Tuning.Gates.CooldownMS = a.CooldownMS
		a.CooldownMS = 0
		note("cooldown_ms", "gates.cooldown_ms")
	}
	if a.PromotionCeiling != nil {
		a.Tuning.Buckets.Critical.Priority = *a.PromotionCeiling
		a.PromotionCeiling = nil
		note("promotion_ceiling", "buckets.critical.priority")
	}
	if a.DemotionFloor != nil {
		a.Tuning.Buckets.Background.Priority = *a.DemotionFloor
		a.DemotionFloor = nil
		note("demotion_floor", "buckets.background.priority")
	}

	return notes
}
