package config

type Config struct {
	Games map[string]Game `json:"games"`
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

// Auto configures the automatic thread governor.
type Auto struct {
	Mode             string      `json:"mode,omitempty"`             // "limited", "full" — handle access level
	Optimisation     string      `json:"optimisation,omitempty"`     // "observe", "manual", "auto"
	Aggression       string      `json:"aggression,omitempty"`       // "conservative", "standard", "aggressive"
	PollIntervalMS   int         `json:"poll_interval_ms,omitempty"` //
	StableWindows    int         `json:"stable_windows,omitempty"`   // consecutive windows before a classification acts
	CooldownMS       int         `json:"cooldown_ms,omitempty"`      // min time between changes to the same thread
	PromotionCeiling *int        `json:"promotion_ceiling,omitempty"`
	DemotionFloor    *int        `json:"demotion_floor,omitempty"`
	CriticalCores    []int       `json:"critical_cores,omitempty"`
	BackgroundCores  []int       `json:"background_cores,omitempty"`
	Exclude          []string    `json:"exclude,omitempty"` // name/module wildcards auto never touches
	Force            []ForceRule `json:"force,omitempty"`
}

// ForceRule pins matching threads to a bucket regardless of classification.
type ForceRule struct {
	Name   string `json:"name,omitempty"`   // thread-name wildcard
	Module string `json:"module,omitempty"` // start-module wildcard (full mode only)
	Bucket string `json:"bucket"`           // "critical", "interactive", "background", "untouchable"
}

const (
	defaultPollIntervalMS   = 1500
	defaultStableWindows    = 3
	defaultCooldownMS       = 30000
	defaultPromotionCeiling = 2  // THREAD_PRIORITY_HIGHEST
	defaultDemotionFloor    = -1 // THREAD_PRIORITY_BELOW_NORMAL
)

func (a *Auto) applyDefaults() {
	if a.Mode == "" {
		a.Mode = "limited"
	}
	if a.Optimisation == "" {
		a.Optimisation = "observe"
	}
	if a.Aggression == "" {
		a.Aggression = "standard"
	}
	if a.PollIntervalMS <= 0 {
		a.PollIntervalMS = defaultPollIntervalMS
	}
	if a.StableWindows <= 0 {
		a.StableWindows = defaultStableWindows
	}
	if a.CooldownMS <= 0 {
		a.CooldownMS = defaultCooldownMS
	}
	if a.PromotionCeiling == nil {
		ceiling := defaultPromotionCeiling
		a.PromotionCeiling = &ceiling
	}
	if a.DemotionFloor == nil {
		floor := defaultDemotionFloor
		a.DemotionFloor = &floor
	}
}
