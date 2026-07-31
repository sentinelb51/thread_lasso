package config

// Tuning is every number and switch the governor consults that is not a
// per-thread rule. It exists so that policy is data the user can see and edit
// rather than constants only a rebuild can change.
//
// The whole struct is always fully populated: Load starts from
// DefaultTuning(aggression) and unmarshals the config file on top, so an absent
// key means "keep the default" and an explicit zero means zero. That is why no
// field here is a pointer or carries omitempty — the two would be
// indistinguishable otherwise, and "eco_qos": false has to be able to mean
// something.
//
// Field tags drive both the settings UI and the generated reference:
//
//	json     the config key (also the source of the display label)
//	desc     one sentence the user reads before changing it
//	min/max  the accepted range, enforced on parse
//	choices  a closed set of string values
//	label    overrides the label derived from the json name
type Tuning struct {
	Buckets BucketActions `json:"buckets" group:"Bucket actions"`
	Roles   RoleActions   `json:"roles" group:"Per-role overrides"`
	Gates   Gates         `json:"gates" group:"Safety gates"`
	Signals Signals       `json:"signals" group:"Classification signals"`
	Scan    Scan          `json:"scan" group:"Identification scanning"`
}

// BucketActions is what each tuning bucket does to a thread. There is no entry
// for "untouchable" or "none" — those buckets exist precisely to mean that
// nothing is applied.
type BucketActions struct {
	Critical    BucketAction `json:"critical" group:"Bucket: critical"`
	Interactive BucketAction `json:"interactive" group:"Bucket: interactive"`
	Background  BucketAction `json:"background" group:"Bucket: background"`
}

// Priority modes. The mode is separate from the target because the same target
// means two different things depending on whether the governor is allowed to
// move a thread down to reach it.
const (
	PriorityOff   = "off"   // never touch this thread's priority
	PriorityRaise = "raise" // move up to the target, never down
	PriorityLower = "lower" // move down to the target, never up
	PrioritySet   = "set"   // force the target either way
)

// CPU-set selections. The lists themselves are per-game hardware layout and
// live in auto.critical_cores / auto.background_cores.
const (
	CpuSetsOff        = "off"
	CpuSetsCritical   = "critical"
	CpuSetsBackground = "background"
)

// I/O priority selections. "off" leaves the thread's I/O priority alone; the
// rest map onto the Windows 0..3 hints.
const (
	IoOff     = "off"
	IoVeryLow = "very_low"
	IoLow     = "low"
	IoNormal  = "normal"
	IoHigh    = "high"
)

// IoPriorityValue maps an io_priority choice to the Windows hint, reporting
// false for "off" and anything unrecognised.
func IoPriorityValue(name string) (int, bool) {
	switch name {
	case IoVeryLow:
		return 0, true
	case IoLow:
		return 1, true
	case IoNormal:
		return 2, true
	case IoHigh:
		return 3, true
	}

	return 0, false
}

// BucketAction is the complete treatment one bucket applies. Every field is
// independent: an action that cannot be applied (no capability, no configured
// core list) is skipped without affecting the others.
type BucketAction struct {
	Priority int `json:"priority" min:"-15" max:"15" desc:"Thread priority relative to the process base, applied according to the mode below. +2 is THREAD_PRIORITY_HIGHEST, -1 is BELOW_NORMAL, -2 is LOWEST."`

	PriorityMode string `json:"priority_mode" choices:"off|raise|lower|set" desc:"raise only ever moves a thread up to the target, lower only ever moves it down, set forces it either way, off leaves priority alone. Use lower to nudge a bucket down without risking raising a thread the game already prioritised."`

	IOPriority string `json:"io_priority" choices:"off|very_low|low|normal|high" desc:"Windows I/O priority hint. Throttling a thread that streams assets or drains a socket stalls it hard, so this is off for most buckets by default."`

	MemoryPriority int `json:"memory_priority" min:"0" max:"5" desc:"Working-set trim order, 1 (reclaimed first) to 5 (normal); 0 means leave it alone. Lowering it makes Windows steal this thread's pages before the game's."`

	EcoQoS bool `json:"eco_qos" desc:"Put the thread on efficiency cores and a lower clock via power throttling. Effective on hybrid CPUs, invisible on uniform ones, and harmful to anything latency-sensitive."`

	CPUSets string `json:"cpu_sets" choices:"off|critical|background" desc:"Restrict the thread to auto.critical_cores or auto.background_cores. A soft preference the scheduler may override under load, unlike hard affinity."`

	IdealCore bool `json:"ideal_core" desc:"Steer the thread onto its own physical core, SMT-aware and assigned once so it keeps its cache warmth. Only worth it for threads that run continuously."`
}

// Lowers reports whether the action reduces anything a thread had. It is what
// decides whether a demotion gate applies and whether the starvation watchdog
// keeps an eye on the thread afterwards, which is why it covers memory and I/O
// and not just priority: a thread throttled into a stall is starved however it
// got there.
func (a BucketAction) Lowers() bool {
	if a.EcoQoS || a.MemoryPriority > 0 || a.CPUSets == CpuSetsBackground {
		return true
	}
	switch a.IOPriority {
	case IoVeryLow, IoLow:
		return true
	}

	return (a.PriorityMode == PriorityLower || a.PriorityMode == PrioritySet) && a.Priority < 0
}

// RoleActions overrides individual fields of a role's bucket action. Buckets
// are deliberately coarse — they are what hysteresis commits on — but a bucket
// can hold roles that want different treatment: pool-idle, telemetry and
// network all sit in background, and throttling a socket pump's I/O is not the
// same decision as throttling a logger's.
//
// Every field is optional. An unset field inherits the bucket's value.
type RoleActions struct {
	MainSim      ActionOverride `json:"main/sim" group:"Role: main/sim"`
	RenderSubmit ActionOverride `json:"render" group:"Role: render"`
	GPUWorker    ActionOverride `json:"gpu-driver" group:"Role: gpu-driver"`
	Audio        ActionOverride `json:"audio" group:"Role: audio"`
	Input        ActionOverride `json:"input" group:"Role: input"`
	Network      ActionOverride `json:"network/voice" group:"Role: network/voice"`
	JobWorker    ActionOverride `json:"job-worker" group:"Role: job-worker"`
	Loader       ActionOverride `json:"loader" group:"Role: loader"`
	PoolIdle     ActionOverride `json:"pool-idle" group:"Role: pool-idle"`
	Telemetry    ActionOverride `json:"telemetry" group:"Role: telemetry"`
}

// ActionOverride is a sparse BucketAction: nil means "inherit". The settings UI
// renders nil as the choice "inherit" so the tri-state needs no special widget.
type ActionOverride struct {
	Priority       *int    `json:"priority,omitempty" min:"-15" max:"15" desc:"Overrides the bucket's priority target for this role alone."`
	PriorityMode   *string `json:"priority_mode,omitempty" choices:"off|raise|lower|set" desc:"Overrides the bucket's priority mode for this role alone."`
	IOPriority     *string `json:"io_priority,omitempty" choices:"off|very_low|low|normal|high" desc:"Overrides the bucket's I/O priority for this role alone. This is how telemetry is throttled while the network threads sharing its bucket are not."`
	MemoryPriority *int    `json:"memory_priority,omitempty" min:"0" max:"5" desc:"Overrides the bucket's memory priority for this role alone."`
	EcoQoS         *bool   `json:"eco_qos,omitempty" desc:"Overrides the bucket's EcoQoS setting for this role alone."`
	CPUSets        *string `json:"cpu_sets,omitempty" choices:"off|critical|background" desc:"Overrides the bucket's CPU-set selection for this role alone."`
	IdealCore      *bool   `json:"ideal_core,omitempty" desc:"Overrides the bucket's ideal-core reservation for this role alone."`
}

// Apply layers the override onto a bucket action, returning the result.
func (o ActionOverride) Apply(base BucketAction) BucketAction {
	if o.Priority != nil {
		base.Priority = *o.Priority
	}
	if o.PriorityMode != nil {
		base.PriorityMode = *o.PriorityMode
	}
	if o.IOPriority != nil {
		base.IOPriority = *o.IOPriority
	}
	if o.MemoryPriority != nil {
		base.MemoryPriority = *o.MemoryPriority
	}
	if o.EcoQoS != nil {
		base.EcoQoS = *o.EcoQoS
	}
	if o.CPUSets != nil {
		base.CPUSets = *o.CPUSets
	}
	if o.IdealCore != nil {
		base.IdealCore = *o.IdealCore
	}

	return base
}

// Gates are the checks between a classification and a change to a thread.
type Gates struct {
	PollIntervalMS int `json:"poll_interval_ms" min:"100" max:"60000" desc:"How often every thread is sampled. Shorter reacts faster to phase changes and costs more CPU; the counters are rates, so accuracy barely moves."`

	StableWindows int `json:"stable_windows" min:"1" max:"60" desc:"Consecutive samples that must agree on a bucket before it is enacted. This is the main defence against acting on a thread that spiked once."`

	CooldownMS int `json:"cooldown_ms" min:"0" max:"600000" desc:"Minimum time between changes to the same thread. Stops a flapping classification turning into a burst of priority writes."`

	SaferBucketFactor float64 `json:"safer_bucket_factor" min:"0.05" max:"1" desc:"Fraction of stable_windows needed to move a thread to a safer bucket. Below 1 makes undoing a demotion cheaper than making one, which is the asymmetry you want: a wrongly demoted thread costs frames every frame."`

	DemoteMinConfidence float64 `json:"demote_min_confidence" min:"0" max:"1" desc:"Classification confidence a thread must reach before anything is lowered. Promotions have no such bar — a wrong promotion costs almost nothing, a wrong demotion costs frames."`

	DemoteRoles []string `json:"demote_roles" desc:"The only roles whose settings may be lowered. Empty means never demote anything. Roles not listed still get their bucket's raising actions."`

	StarvationReadyRatio float64 `json:"starvation_ready_ratio" min:"0.01" max:"1" desc:"Share of samples a lowered thread is seen runnable-but-not-running before the watchdog reverts it and stops touching it for the session. Lower is more trigger-happy about undoing demotions."`

	QuarantineOnRollback bool `json:"quarantine_on_rollback" desc:"After a starvation rollback, never touch that thread again this session. Turning this off lets the governor retry, which can oscillate if the classification is genuinely wrong."`
}

// Signals are the thresholds the classifier reads behaviour against. Cycle
// rates assume multi-GHz cores; they separate "parked" from "doing real work"
// rather than expressing exact CPU percentages.
//
// Most rules are ramps rather than cuts: a pair of _lo/_hi bounds where the
// evidence fades in linearly between them. Setting the two equal turns a ramp
// back into a hard threshold.
type Signals struct {
	ParkedCyclesRate float64 `json:"parked_cycles_rate" min:"0" desc:"Cycles per second below which a thread is doing nothing at all. Roughly 0.02% of one core at 4.5 GHz."`

	ActiveCyclesRate float64 `json:"active_cycles_rate" min:"0" desc:"Cycles per second above which a thread counts as working rather than ticking over. Roughly 1% of one core at 4.5 GHz."`

	PoolIdleSwitchRate float64 `json:"pool_idle_switch_rate" min:"0" desc:"Wakes per second below which a pool member is idling between jobs rather than servicing a queue."`

	PumpSwitchRate float64 `json:"pump_switch_rate" min:"0" desc:"Wakes per second above which a thread looks like a pump — audio, network or input — rather than a worker."`

	PumpMaxQuantum float64 `json:"pump_max_quantum" min:"0" desc:"Cycles per wake below which a thread is doing a sliver of work per wake. A pump is defined by both this and the wake rate; a thread that wakes often and works hard is not one."`

	RegularWakeCV float64 `json:"regular_wake_cv" min:"0" max:"5" desc:"Coefficient of variation in wake spacing below which a thread is metronomic. Reinforces the audio reading of a pump."`

	BurstyWakeCV float64 `json:"bursty_wake_cv" min:"0" max:"10" desc:"Wake-spacing variation above which a thread works in bursts — the streaming and asset-loading signature."`

	MinScore float64 `json:"min_score" min:"0.5" max:"20" desc:"Total evidence a role must accumulate before it is reported at all. Below it the thread stays unknown and is never touched. Raising this makes the governor act on fewer threads with more certainty."`

	PoolWaitShare float64 `json:"pool_wait_share" min:"0" max:"1" desc:"Share of a thread's waits spent on a parking primitive above which that primitive is the dominant story for the thread."`

	PoolCohortLo float64 `json:"pool_cohort_lo" min:"1" desc:"Cohort size at which a thread starts reading as a pool member rather than a role the process has one of. Below this it is treated as unique."`

	PoolCohortHi float64 `json:"pool_cohort_hi" min:"1" desc:"Cohort size at which a thread reads as a pool member outright. A process has exactly one main thread, so pool evidence is what rules main/sim out."`

	GameElevatedPriority int `json:"game_elevated_priority" min:"1" max:"15" desc:"Baseline priority above the process base at which the game is judged to have deliberately prioritised a thread, making it untouchable. Measured at first sighting, never live."`

	CadenceLo float64 `json:"cadence_lo" min:"1" desc:"Bottom of the display-cadence band, in wakes per second. A thread waking steadily inside this band while carrying real work is running on the swapchain."`

	CadenceHi float64 `json:"cadence_hi" min:"1" desc:"Top of the display-cadence band. Generous by default because a present path wakes more than once per frame."`

	CadenceCV float64 `json:"cadence_cv" min:"0" max:"5" desc:"Wake-spacing variation below which a thread counts as running on a refresh interval rather than on work arriving."`

	IoPendingShare float64 `json:"io_pending_share" min:"0" max:"1" desc:"Share of samples with an outstanding I/O request above which a thread is waiting on a device rather than on the process. Paired with the stack modules this is the closest Windows gets to a per-thread network counter."`

	HotShareLo float64 `json:"hot_share_lo" min:"0" max:"1" desc:"Cycle share, relative to the hottest thread, at which frame-critical evidence starts to count."`

	HotShareHi float64 `json:"hot_share_hi" min:"0" max:"1" desc:"Cycle share at which a thread is unambiguously one of the process's hot set."`

	CruncherRateLo float64 `json:"cruncher_rate_lo" min:"0" desc:"Absolute cycle rate at which sustained-work evidence starts to count."`

	CruncherRateHi float64 `json:"cruncher_rate_hi" min:"0" desc:"Absolute cycle rate at which a thread is working continuously."`

	CruncherQuantumLo float64 `json:"cruncher_quantum_lo" min:"0" desc:"Cycles per wake at which long-quantum evidence starts to count — simulation and heavy worker code, as opposed to a pump."`

	CruncherQuantumHi float64 `json:"cruncher_quantum_hi" min:"0" desc:"Cycles per wake at which a thread is unambiguously doing long uninterrupted work."`

	RenderShareLo float64 `json:"render_share_lo" min:"0" max:"1" desc:"Cycle share at which render-submit evidence starts to count. Render threads deliver a large share of the cycles in many small wakes, so this pairs with the switch-rate band below."`

	RenderShareHi float64 `json:"render_share_hi" min:"0" max:"1" desc:"Cycle share at which the render-submit reading is fully supported."`

	RenderSwitchLo float64 `json:"render_switch_lo" min:"0" desc:"Wakes per second at which render-submit evidence starts to count."`

	RenderSwitchHi float64 `json:"render_switch_hi" min:"0" desc:"Wakes per second at which the render-submit reading is fully supported."`

	FrameCorrelationLo float64 `json:"frame_correlation_lo" min:"0" max:"1" desc:"Correlation with the hottest thread's load envelope at which frame-coupling evidence starts. At poll-interval granularity this is a load correlation, not true frame coupling, so it nudges rather than decides."`

	FrameCorrelationHi float64 `json:"frame_correlation_hi" min:"0" max:"1" desc:"Correlation at which frame coupling is fully supported."`

	TelemetryMinLifetimeS float64 `json:"telemetry_min_lifetime_s" min:"0" desc:"How long a sleep-looping thread must have existed before it can be called telemetry. Stops a thread that happens to be sleeping during startup being demoted."`

	TelemetryShareLo float64 `json:"telemetry_share_lo" min:"0" max:"1" desc:"Cycle share above which telemetry evidence starts to be discounted — a thread doing real work is not a logger, however much it sleeps."`

	TelemetryShareHi float64 `json:"telemetry_share_hi" min:"0" max:"1" desc:"Cycle share at which telemetry evidence is discounted entirely."`

	LoaderShareLo float64 `json:"loader_share_lo" min:"0" max:"1" desc:"Bottom of the cycle-share band a streaming or asset-loading thread occupies."`

	LoaderShareHi float64 `json:"loader_share_hi" min:"0" max:"1" desc:"Top of that band. Above it the thread is a worker, not a loader."`

	GPUDriverShare float64 `json:"gpu_driver_share" min:"0" max:"1" desc:"Cycle share above which a thread that started in a vendor display driver is an active GPU worker rather than a parked driver pool such as a shader compiler."`

	GPUDriverSwitchRate float64 `json:"gpu_driver_switch_rate" min:"0" desc:"Wake rate that qualifies a driver thread as active regardless of its cycle share."`

	CadenceShareLo float64 `json:"cadence_share_lo" min:"0" max:"1" desc:"Cycle share at which display-cadence evidence starts to count. This is what keeps an audio mixer, which is just as metronomic, from reading as a present thread."`

	CadenceShareHi float64 `json:"cadence_share_hi" min:"0" max:"1" desc:"Cycle share at which display cadence is fully supported."`

	GuiPumpMaxShare float64 `json:"gui_pump_max_share" min:"0" max:"1" desc:"Cycle share below which a thread holding a Win32 thread info block reads as an input pump. Plenty of threads touch a window once, so this only nudges, and only for cheap threads."`
}

// Scan budgets the process-memory reads that identify threads. These cost
// ReadProcessMemory calls against the game, so they are spread across ticks
// rather than run on every thread every time.
type Scan struct {
	StackIntervalS float64 `json:"stack_interval_s" min:"0" desc:"How long a thread's stack fingerprint is kept before it is swept again. What a thread does changes far more slowly than what it is doing right now."`

	StackScansPerTick int `json:"stack_scans_per_tick" min:"0" max:"512" desc:"Threads swept per sample. A full pass over a large process spans several ticks; raising this identifies threads sooner at the cost of more reads per tick. 0 disables stack scanning entirely."`

	StackStartupWindowKB int `json:"stack_startup_window_kb" min:"1" max:"1024" desc:"Bytes read from the top of a thread's stack to recover the routine it was created with. The start frames sit at the very top and are never overwritten."`

	StackActiveWindowKB int `json:"stack_active_window_kb" min:"4" max:"4096" desc:"Bytes read from the live end of the stack to fingerprint what the thread is doing. Larger sees deeper history at proportionally more cost."`

	StackMinHits int `json:"stack_min_hits" min:"1" max:"32" desc:"Pointers into a module needed before it counts as being on the stack. One pointer is as likely to be a stale argument as a return address; two is a call path."`

	StackMaxModules int `json:"stack_max_modules" min:"1" max:"64" desc:"How many modules to keep per thread, most frames first."`

	ModuleReloadIntervalS float64 `json:"module_reload_interval_s" min:"0" desc:"How long to wait between rebuilds of the module table while addresses are still failing to resolve. A protected process finishes mapping long after a scanner first sees it."`

	ModuleReloadLimit int `json:"module_reload_limit" min:"0" max:"1000" desc:"How many rebuilds may find nothing new before the governor stops trying. Addresses that never resolve are manually mapped code, which is exactly what a protection leaves behind."`
}

// Aggression levels. Aggression is a preset: it selects the defaults for the
// whole tuning table rather than gating actions behind extra conditions, so
// what a level does is visible in the settings rather than buried in the
// actuator.
const (
	AggressionConservative = "conservative"
	AggressionStandard     = "standard"
	AggressionAggressive   = "aggressive"
)

// DefaultTuning returns the preset for an aggression level. Unknown levels get
// the standard preset, which is also what an unset "aggression" resolves to.
//
// These three tables reproduce the behaviour each level had when it was
// hardcoded, which is the contract: upgrading must not change what the tool
// does to anyone's machine. Everything below is now visible and overridable.
func DefaultTuning(aggression string) Tuning {
	t := Tuning{
		Gates: Gates{
			PollIntervalMS:       1500,
			StableWindows:        3,
			CooldownMS:           30000,
			SaferBucketFactor:    0.5,
			DemoteMinConfidence:  0.5,
			StarvationReadyRatio: 0.25,
			QuarantineOnRollback: true,
		},
		Signals: defaultSignals(),
		Scan:    defaultScan(),
	}

	// Critical is the same at every level except that raising I/O priority —
	// which competes with the rest of the system, not just the game — waits for
	// the level that opted into competing.
	t.Buckets.Critical = BucketAction{
		Priority:     2,
		PriorityMode: PriorityRaise,
		IOPriority:   IoOff,
		CPUSets:      CpuSetsCritical,
		IdealCore:    true,
	}
	// Interactive guarantees only that a thread is not stuck below normal.
	t.Buckets.Interactive = BucketAction{
		Priority:     0,
		PriorityMode: PriorityRaise,
		IOPriority:   IoOff,
		CPUSets:      CpuSetsOff,
	}
	t.Buckets.Background = BucketAction{
		Priority:     -1,
		PriorityMode: PriorityLower,
		IOPriority:   IoOff,
		CPUSets:      CpuSetsOff,
	}

	switch aggression {
	case AggressionConservative:
		// Raise-only: nothing is ever lowered, so the demote list is empty and
		// the background bucket has nothing to do.
		t.Gates.DemoteRoles = nil
		t.Buckets.Background.PriorityMode = PriorityOff

	case AggressionAggressive:
		t.Gates.DemoteRoles = allDemotableRoles()
		t.Buckets.Critical.IOPriority = IoHigh
		t.Buckets.Background.MemoryPriority = 3
		t.Buckets.Background.CPUSets = CpuSetsBackground
		// Telemetry is the one role in the background bucket it is safe to
		// throttle. A socket pump or an asset loader that lands there must still
		// reach the device at full speed, which is what the override expresses.
		t.Roles.Telemetry = ActionOverride{
			IOPriority: stringPtr(IoLow),
			EcoQoS:     boolPtr(true),
		}

	default: // standard
		// Demote only the two roles whose classification is hardest to get
		// wrong, and only their priority.
		t.Gates.DemoteRoles = []string{"pool-idle", "telemetry"}
	}

	return t
}

// allDemotableRoles is every role except the ones the governor promotes. Listed
// explicitly rather than derived so that adding a role does not silently make
// it demotable.
func allDemotableRoles() []string {
	return []string{"audio", "job-worker", "loader", "network/voice", "pool-idle", "telemetry"}
}

func defaultSignals() Signals {
	return Signals{
		ParkedCyclesRate:      1e6,
		ActiveCyclesRate:      5e7,
		PoolIdleSwitchRate:    100,
		PumpSwitchRate:        300,
		PumpMaxQuantum:        3e4,
		RegularWakeCV:         0.5,
		BurstyWakeCV:          1.5,
		MinScore:              2.0,
		PoolWaitShare:         0.5,
		PoolCohortLo:          1.5,
		PoolCohortHi:          3.5,
		GameElevatedPriority:  2,
		CadenceLo:             45,
		CadenceHi:             700,
		CadenceCV:             0.3,
		IoPendingShare:        0.5,
		HotShareLo:            0.30,
		HotShareHi:            0.60,
		CruncherRateLo:        2e8,
		CruncherRateHi:        1e9,
		CruncherQuantumLo:     2e5,
		CruncherQuantumHi:     1e6,
		RenderShareLo:         0.15,
		RenderShareHi:         0.35,
		RenderSwitchLo:        1000,
		RenderSwitchHi:        3000,
		FrameCorrelationLo:    0.6,
		FrameCorrelationHi:    0.9,
		TelemetryMinLifetimeS: 60,
		TelemetryShareLo:      0.005,
		TelemetryShareHi:      0.05,
		LoaderShareLo:         0.02,
		LoaderShareHi:         0.3,
		GPUDriverShare:        0.15,
		GPUDriverSwitchRate:   5000,
		CadenceShareLo:        0.05,
		CadenceShareHi:        0.25,
		GuiPumpMaxShare:       0.1,
	}
}

func defaultScan() Scan {
	return Scan{
		StackIntervalS:        8,
		StackScansPerTick:     12,
		StackStartupWindowKB:  4,
		StackActiveWindowKB:   96,
		StackMinHits:          2,
		StackMaxModules:       8,
		ModuleReloadIntervalS: 15,
		ModuleReloadLimit:     8,
	}
}

// ActionFor resolves the treatment for one classified thread: the bucket's
// action with the role's overrides layered on top.
func (t *Tuning) ActionFor(bucket string, role string) (BucketAction, bool) {
	var action BucketAction
	switch bucket {
	case "critical":
		action = t.Buckets.Critical
	case "interactive":
		action = t.Buckets.Interactive
	case "background":
		action = t.Buckets.Background
	default:
		return BucketAction{}, false
	}

	if override, ok := t.Roles.byName(role); ok {
		action = override.Apply(action)
	}

	return action, true
}

// Demotable reports whether a role's settings may be lowered at all.
func (g *Gates) Demotable(role string) bool {
	for _, name := range g.DemoteRoles {
		if name == role {
			return true
		}
	}

	return false
}

// byName maps a role name to its override. The names are the ones the UI shows
// and the same ones auto.role_buckets uses; config cannot import governor, so
// they are duplicated here and a test asserts the two lists agree.
func (r *RoleActions) byName(role string) (ActionOverride, bool) {
	switch role {
	case "main/sim":
		return r.MainSim, true
	case "render":
		return r.RenderSubmit, true
	case "gpu-driver":
		return r.GPUWorker, true
	case "audio":
		return r.Audio, true
	case "input":
		return r.Input, true
	case "network/voice":
		return r.Network, true
	case "job-worker":
		return r.JobWorker, true
	case "loader":
		return r.Loader, true
	case "pool-idle":
		return r.PoolIdle, true
	case "telemetry":
		return r.Telemetry, true
	}

	return ActionOverride{}, false
}

// RoleNames lists every role the tuning table knows, in the order the settings
// UI presents them.
func RoleNames() []string {
	return []string{
		"main/sim", "render", "gpu-driver", "audio", "input",
		"network/voice", "job-worker", "loader", "pool-idle", "telemetry",
	}
}

func stringPtr(s string) *string { return &s }
func boolPtr(b bool) *bool       { return &b }
