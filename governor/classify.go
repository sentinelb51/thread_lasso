//go:build windows && amd64

package governor

import (
	"ThreadOrchestra/config"
	"ThreadOrchestra/process"
	"ThreadOrchestra/thread"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
)

type Role int

const (
	RoleUnknown Role = iota
	RoleMainSim
	RoleRenderSubmit
	RoleGPUWorker
	RoleAudio
	RoleInput
	RoleNetwork // includes voice chat
	RoleJobWorker
	RoleLoader
	RolePoolIdle
	RoleTelemetry
)

// roleCount bounds the score arrays, and the declaration order above is both
// the scoring order and the tie-break order: two roles tied at the top resolve
// to the earlier (more specific) one. Ranking used to walk a map, so ties
// swapped winner at random every window; the hysteresis streak then reset every
// tick and the thread could never reach a stable verdict, meaning it was never
// actuated. Fixed-size arrays make the order structural — it cannot drift out
// of sync with a hand-maintained slice — and keep scoring allocation-free, which
// matters because every thread is rescored on every tick.
const roleCount = int(RoleTelemetry) + 1

var roleNames = map[Role]string{
	RoleUnknown:      "unknown",
	RoleMainSim:      "main/sim",
	RoleRenderSubmit: "render",
	RoleGPUWorker:    "gpu-driver",
	RoleAudio:        "audio",
	RoleInput:        "input",
	RoleNetwork:      "network/voice",
	RoleJobWorker:    "job-worker",
	RoleLoader:       "loader",
	RolePoolIdle:     "pool-idle",
	RoleTelemetry:    "telemetry",
}

func (r Role) String() string { return roleNames[r] }

type Bucket int

const (
	BucketNone Bucket = iota // unknown → leave alone
	BucketUntouchable
	BucketCritical
	BucketInteractive
	BucketBackground
)

var bucketNames = map[Bucket]string{
	BucketNone:        "-",
	BucketUntouchable: "untouchable",
	BucketCritical:    "critical",
	BucketInteractive: "interactive",
	BucketBackground:  "background",
}

func (b Bucket) String() string { return bucketNames[b] }

// Facts is the complete evidence about one thread for a single window.
type Facts struct {
	Series       *Series
	Description  string
	Module       string  // lowercase base name of the entry point's module; "" when unknown
	ModuleOffset uintptr // offset of the entry point into Module

	// Stack holds the modules with frames on the thread's stack, most frames
	// first. Where a thread *started* is a fact about how it was created; what
	// is on its stack is a fact about what it does, and for a pool worker handed
	// graphics or socket work the second is the only useful one.
	Stack []string

	// GUIThread is set when the thread has a TEB.Win32ThreadInfo block, i.e. has
	// called into user32/gdi32 at least once.
	GUIThread bool

	TimeCritical      bool // the *game* elevated this thread (see Series.BaselineRelative)
	PriorityBoosted   bool // the game nudged it above the process base, but not to the top
	IsForegroundInput bool
	CohortWeight      float64 // how many threads look like this one (see buildCohorts)
	CyclesShare       float64 // CyclesRateLong / hottest thread's rate
	FrameCorrelation  float64 // vs the hottest game thread; 0 for the hottest itself

	// Signals are the thresholds this window is judged against, resolved by the
	// governor from the config. Nil falls back to the standard preset, which is
	// what keeps ClassifyRole usable from tests and one-shot tools.
	Signals *config.Signals

	// Config overrides, resolved by the governor from the auto.exclude and
	// auto.force rules before classification is bucketed.
	Excluded     bool   // matched an exclude glob — never touch this thread
	ForcedBucket Bucket // a force rule pins the bucket; BucketNone = not forced
}

// fallbackSignals is what an unconfigured Facts is scored against. Every
// threshold the classifier reads now lives in config.Signals; this exists so
// that a Facts built by hand — a test, the probe — still scores the same way
// the governor would score it.
var fallbackSignals = config.DefaultTuning(config.AggressionStandard).Signals

// signals resolves the thresholds for one window.
func (f *Facts) signals() *config.Signals {
	if f.Signals != nil {
		return f.Signals
	}

	return &fallbackSignals
}

// Module names that identify a subsystem. They are matched as substrings of a
// lowercase base name, so "d3d11" catches d3d11.dll and d3d11on12.dll alike.
var (
	gpuDriverModules = []string{
		"amdxx64", "atidxx64", "nvwgf2umx", "nvd3dumx", "nvoglv64", "igd10iumd64", "igd12umd64",
	}

	// The D3D/DXGI runtime itself, as opposed to the vendor driver below it.
	graphicsModules = []string{"dxgi", "d3d9", "d3d11", "d3d12", "d3dcompiler", "dcomp", "dxcore"}

	// win32u is the syscall stub layer for the graphics kernel. Frames from it
	// underneath dxgi are a present or a command submission on its way to
	// dxgkrnl — which is to say, this thread is the one handing finished frames
	// to the display.
	graphicsKernelModules = []string{"win32u", "gdi32"}

	audioModules = []string{"bink", "xaudio", "mmdevapi", "audioses", "wasapi", "avrt", "fmod", "wwise"}

	networkModules = []string{
		"vivoxsdk", "mswsock", "ws2_32", "winhttp", "wininet",
		"dnsapi", "iphlpapi", "nsi.dll", "fwpuclnt", "rasadhlp",
	}

	inputModules = []string{"xinput", "dinput8", "hid.dll", "gameinput"}
)

// nameHints maps thread-name tokens (from SetThreadDescription) to roles.
// Matching is per token with prefix semantics rather than a substring search of
// the whole name: "Logic" is not a logging thread, but strings.Contains on "log"
// said it was, pushing an engine's main logic thread toward telemetry — a
// demotable bucket. Hints that are also common word stems are marked whole so
// they only match a token outright.
var nameHints = []struct {
	token  string
	role   Role
	weight float64
	whole  bool
}{
	{token: "bink", role: RoleAudio, weight: 3},
	{token: "audio", role: RoleAudio, weight: 4},
	{token: "sound", role: RoleAudio, weight: 4},
	{token: "mixer", role: RoleAudio, weight: 3},
	{token: "render", role: RoleRenderSubmit, weight: 4},
	{token: "rhi", role: RoleRenderSubmit, weight: 3, whole: true},
	{token: "gfx", role: RoleRenderSubmit, weight: 3},
	{token: "present", role: RoleRenderSubmit, weight: 3},
	{token: "input", role: RoleInput, weight: 4},
	{token: "net", role: RoleNetwork, weight: 3},
	{token: "sock", role: RoleNetwork, weight: 3},
	{token: "voice", role: RoleNetwork, weight: 3},
	{token: "job", role: RoleJobWorker, weight: 2},
	{token: "worker", role: RoleJobWorker, weight: 2},
	{token: "task", role: RoleJobWorker, weight: 1.5},
	{token: "pool", role: RoleJobWorker, weight: 1.5},
	{token: "loader", role: RoleLoader, weight: 3},
	{token: "stream", role: RoleLoader, weight: 2},
	{token: "telemetry", role: RoleTelemetry, weight: 4},
	// "log" must match on its own only — it is a prefix of "logic", which names
	// a main thread, not a logging one. The -gg- forms have no such collision.
	{token: "log", role: RoleTelemetry, weight: 1.5, whole: true},
	{token: "logg", role: RoleTelemetry, weight: 1.5},
}

// scoreSheet accumulates per-role evidence for one window. hits counts how many
// independent rules backed each role, which is what separates "three signals
// agree" from "one signal fired and nothing contradicted it" — the latter used
// to be reported as 100% confidence.
type scoreSheet struct {
	score [roleCount]float64
	hits  [roleCount]int

	// minScore travels with the sheet because winner and confidence are read
	// after scoring, by callers that no longer have the Facts to hand.
	minScore float64
}

// add records points for a role. Graded rules routinely evaluate to zero, and
// a zero-weight rule is not evidence, so it counts as neither score nor hit.
func (sheet *scoreSheet) add(role Role, points float64) {
	if points <= 0 {
		return
	}

	sheet.score[role] += points
	sheet.hits[role]++
}

// best returns the top-scoring role in deterministic order. See roleCount.
func (sheet *scoreSheet) best() Role {
	best := RoleUnknown
	for role := Role(1); int(role) < roleCount; role++ {
		if sheet.score[role] > sheet.score[best] {
			best = role
		}
	}

	return best
}

// winner is the top-ranked role once it clears the minimum score.
func (sheet *scoreSheet) winner() Role {
	best := sheet.best()
	if best == RoleUnknown || sheet.score[best] < sheet.minScore {
		return RoleUnknown
	}

	return best
}

// confidence rates how well the evidence supports a specific role — the role
// actually being reported, which is not always this window's winner.
func (sheet *scoreSheet) confidence(role Role) float64 {
	if role == RoleUnknown {
		return 0
	}

	best := sheet.score[role]
	if best < sheet.minScore {
		return 0
	}

	// The runner-up is floored: an unopposed signal is not certainty, it is
	// just the only thing that fired. Without the floor, the *least* informed
	// classifications were the ones reporting 100%.
	rival := sheet.minScore / 2
	for other := Role(1); int(other) < roleCount; other++ {
		if other != role && sheet.score[other] > rival {
			rival = sheet.score[other]
		}
	}

	margin := clamp01((best - rival) / best)
	support := clamp01(float64(sheet.hits[role]) / 2)

	return margin * (0.5 + 0.5*support)
}

// scoreRoles grades every role against the evidence. Rules are graded rather
// than gated: a thread at 0.23 cycles-share is not categorically different from
// one at 0.26, so evidence ramps in over a band instead of switching on at a
// threshold. Pure function.
func scoreRoles(f *Facts) scoreSheet {
	var sheet scoreSheet
	sig := f.signals()
	sheet.minScore = sig.MinScore
	s := f.Series

	// EMAs and the wait histogram carry no signal in the first windows; a
	// fresh thread would otherwise match the "parked" rule spuriously.
	if s == nil || s.Samples < 3 {
		return sheet
	}

	// Hard evidence first: the thread pumping the foreground window's
	// message queue is the input thread.
	if f.IsForegroundInput {
		sheet.add(RoleInput, 10)
	}

	// A pool member cannot be the main thread — a process has exactly one. The
	// cohort weight is itself an estimate, so it dials the single-instance and
	// pool readings of the same evidence against each other rather than
	// switching between them.
	pooled := ramp(f.CohortWeight, sig.PoolCohortLo, sig.PoolCohortHi)
	solo := 1 - pooled

	// Hot crunchers: the top cycle consumers are the frame-critical set.
	hot := ramp(f.CyclesShare, sig.HotShareLo, sig.HotShareHi)
	sheet.add(RoleJobWorker, 2*pooled*hot)
	sheet.add(RoleMainSim, 3*solo*hot)
	if s.CreatedAtStart {
		sheet.add(RoleMainSim, solo) // a founder thread, created with the process
	}

	// Long uninterrupted work quanta at a high absolute rate: simulation or
	// heavy worker code, as opposed to a pump doing a sliver of work per wake.
	cruncher := math.Min(
		ramp(s.CyclesRateLong, sig.CruncherRateLo, sig.CruncherRateHi),
		ramp(s.CyclesPerSwitch, sig.CruncherQuantumLo, sig.CruncherQuantumHi),
	)
	sheet.add(RoleJobWorker, 1.5*pooled*cruncher)
	sheet.add(RoleMainSim, 1.5*solo*cruncher)

	// Render submit: a large share of the cycles delivered in many small wakes.
	// Both conditions must hold, so the weaker of the two ramps governs.
	sheet.add(RoleRenderSubmit, 2.5*math.Min(
		ramp(f.CyclesShare, sig.RenderShareLo, sig.RenderShareHi),
		ramp(s.SwitchRateLong, sig.RenderSwitchLo, sig.RenderSwitchHi),
	))

	// Frame-coupled activity marks render/sim helpers even without modules.
	// At poll-interval granularity this is a load-envelope correlation, not
	// true frame coupling, so it nudges rather than decides.
	coupled := ramp(f.FrameCorrelation, sig.FrameCorrelationLo, sig.FrameCorrelationHi)
	sheet.add(RoleRenderSubmit, 2*coupled)
	sheet.add(RoleMainSim, 1*coupled)

	// Pumps: many tiny wakes. The game marking a pump Time Critical is a
	// strong audio signal (see TID 11068 in the captures).
	isPump := s.SwitchRateLong > sig.PumpSwitchRate && s.CyclesPerSwitch > 0 && s.CyclesPerSwitch < sig.PumpMaxQuantum
	if isPump {
		sheet.add(RoleAudio, 2)
		sheet.add(RoleNetwork, 1.5)
		if s.WakeRegularity() < sig.RegularWakeCV {
			sheet.add(RoleAudio, 1)
		}
		if f.TimeCritical {
			sheet.add(RoleAudio, 2)
		}
	}
	if f.PriorityBoosted && !f.TimeCritical {
		sheet.add(RoleAudio, 1)
		sheet.add(RoleNetwork, 1)
	}

	// Parking primitives. WrQueue is the classic thread-pool KQUEUE wait, but
	// no modern engine is limited to it: WrAlertByThreadId is
	// NtWaitForAlertByThreadId, the futex behind WaitOnAddress / SRWLOCK /
	// condition variables, and it is where Overwatch's job system parks. Keying
	// only on WrQueue made RoleJobWorker and RolePoolIdle unreachable on such a
	// process. The futex is weighted lower because a busy thread blocked on a
	// contended lock waits there too, and coverage discounts a dominant reason
	// that rests on very few observations.
	parked := (s.WaitShare(process.WrQueue) + 0.75*s.WaitShare(process.WrAlertByThreadId)) * s.WaitCoverage()
	if parked > sig.PoolWaitShare {
		switch {
		case s.CyclesRateLong < sig.ParkedCyclesRate:
			sheet.add(RolePoolIdle, 4*parked)
		case s.CyclesRateLong < sig.ActiveCyclesRate && s.SwitchRateLong < sig.PoolIdleSwitchRate:
			sheet.add(RolePoolIdle, 3*parked)
		default:
			sheet.add(RoleJobWorker, 4*parked)
		}
	}

	// A cohort of behaviourally identical threads is a pool of some kind; which
	// kind is left to the rules above.
	sheet.add(RoleJobWorker, pooled)
	sheet.add(RolePoolIdle, pooled)

	// Sleep loops that never do real work. The kernel reports plain
	// DelayExecution for a user-mode Sleep and WrDelayExecution for the waiting
	// variant; checking only the latter missed every Sleep-looping thread.
	sleeping := s.WaitShareAny(process.DelayExecution, process.WrDelayExecution) * s.WaitCoverage()
	if sleeping > sig.PoolWaitShare && s.Lifetime.Seconds() > sig.TelemetryMinLifetimeS {
		sheet.add(RoleTelemetry, 3*sleeping*(1-ramp(f.CyclesShare, sig.TelemetryShareLo, sig.TelemetryShareHi)))
	}

	// Parked regardless of wait reason.
	if s.SwitchRateLong < 1 && s.CyclesRateLong < sig.ParkedCyclesRate {
		sheet.add(RolePoolIdle, 2.5)
	}

	// Bursty, kernel-heavy, mid-activity: streaming/asset loading.
	if s.WakeRegularity() > sig.BurstyWakeCV && s.UserRatio < 0.5 &&
		f.CyclesShare > sig.LoaderShareLo && f.CyclesShare < sig.LoaderShareHi {
		sheet.add(RoleLoader, 1.5)
	}

	// Display cadence: a thread waking at a steady rate inside the plausible
	// refresh band, while carrying real work, is running on the swapchain rather
	// than on work arriving. Audio pumps are metronomic too, which is why this
	// needs the cycle share — a mixer's wake is a few microseconds of work, a
	// present is a frame's worth.
	if s.SwitchRateLong > sig.CadenceLo && s.SwitchRateLong < sig.CadenceHi &&
		s.WakeRegularity() < sig.CadenceCV {
		sheet.add(RoleRenderSubmit, 2*ramp(f.CyclesShare, sig.CadenceShareLo, sig.CadenceShareHi))
	}

	// Entry-point module evidence (full mode only): what the thread was created
	// to do.
	if f.Module != "" {
		switch {
		case matchesAny(f.Module, gpuDriverModules):
			if f.CyclesShare > sig.GPUDriverShare || s.SwitchRateLong > sig.GPUDriverSwitchRate {
				sheet.add(RoleGPUWorker, 4)
			} else {
				sheet.add(RolePoolIdle, 2) // parked driver pool (shader compilers)
			}
		case matchesAny(f.Module, audioModules):
			sheet.add(RoleAudio, 4)
		case matchesAny(f.Module, networkModules):
			sheet.add(RoleNetwork, 4)
		case strings.HasPrefix(f.Module, "ntdll"):
			sheet.add(RolePoolIdle, 1.5)
		}
	}

	scoreStack(&sheet, f)

	// Pending I/O says the thread is waiting on a device rather than on the
	// process; the stack says which device. Windows has no per-thread network
	// counters at all, so this pairing is the closest thing to one.
	if s.IoPendingRatio > sig.IoPendingShare {
		if stackHas(f.Stack, networkModules) {
			sheet.add(RoleNetwork, 2)
		} else {
			sheet.add(RoleLoader, 1)
		}
	}

	// A thread with a Win32 thread info block has called into user32 or gdi32.
	// On its own that is weak — plenty of threads touch a window once — so it
	// only nudges, and only when the thread is also cheap enough to be a pump.
	if f.GUIThread && f.CyclesShare < sig.GuiPumpMaxShare {
		sheet.add(RoleInput, 1.5)
	}

	// Thread-name evidence.
	scoreName(&sheet, f.Description)

	return sheet
}

// scoreStack folds the thread's stack fingerprint into the sheet.
//
// This is the only evidence that survives a process hiding where its threads
// started, and it answers a better question anyway: a job worker currently
// inside the D3D runtime is doing render work no matter which pool created it.
// Frames left behind by earlier calls count too — nothing rewrites the stack
// below the current frame — so a parked thread still shows the subsystem it was
// last inside, which is what makes the signal usable at a 1.5s poll interval.
func scoreStack(sheet *scoreSheet, f *Facts) {
	if len(f.Stack) == 0 {
		return
	}

	// Driver over runtime: a thread down in the vendor UMD is doing the GPU's
	// work, while one that only reaches the runtime is submitting to it.
	switch {
	case stackHas(f.Stack, gpuDriverModules):
		sheet.add(RoleGPUWorker, 4)
	case stackHas(f.Stack, graphicsModules):
		sheet.add(RoleRenderSubmit, 3.5)
	}

	// The flip path: the runtime above the graphics-kernel stubs.
	if stackHas(f.Stack, graphicsModules) && stackHas(f.Stack, graphicsKernelModules) {
		sheet.add(RoleRenderSubmit, 2)
	}

	if stackHas(f.Stack, audioModules) {
		sheet.add(RoleAudio, 4)
	}
	if stackHas(f.Stack, networkModules) {
		sheet.add(RoleNetwork, 4)
	}
	if stackHas(f.Stack, inputModules) {
		sheet.add(RoleInput, 3)
	}
}

// stackHas reports whether any module on the stack matches one of the names.
func stackHas(stack []string, names []string) bool {
	for _, module := range stack {
		if matchesAny(module, names) {
			return true
		}
	}

	return false
}

// scoreName folds thread-name evidence into the sheet. Each hint fires at most
// once however many tokens it matches, so "RenderThread_Render2" is not three
// times the evidence of "Render".
func scoreName(sheet *scoreSheet, description string) {
	if description == "" {
		return
	}

	tokens := identifierTokens(description)
	if len(tokens) == 0 {
		return
	}

	for _, hint := range nameHints {
		for _, token := range tokens {
			if token == hint.token || (!hint.whole && strings.HasPrefix(token, hint.token)) {
				sheet.add(hint.role, hint.weight)
				break
			}
		}
	}
}

// identifierTokens splits a thread description into lowercase words, breaking on
// separators, camelCase boundaries and letter/digit transitions, so
// "GPUWorker_12" becomes {"gpu", "worker", "12"}. Thread names are identifiers,
// and matching whole words in an identifier is far less noisy than matching
// substrings of the raw string.
func identifierTokens(name string) []string {
	runes := []rune(name)
	tokens := make([]string, 0, 4)
	start := -1

	flush := func(end int) {
		if start >= 0 && end > start {
			tokens = append(tokens, strings.ToLower(string(runes[start:end])))
		}
		start = -1
	}

	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush(i)
			continue
		}
		// start >= 0 guarantees runes[i-1] exists and is a word rune.
		if start >= 0 && (startsCamelWord(runes, i) || unicode.IsDigit(r) != unicode.IsDigit(runes[i-1])) {
			flush(i)
		}
		if start < 0 {
			start = i
		}
	}
	flush(len(runes))

	return tokens
}

// startsCamelWord reports whether runes[i] begins a new camelCase word.
// "GPUWorker" must split at the 'W' only — an acronym run is one word, and the
// last capital of the run belongs to the word that follows it.
func startsCamelWord(runes []rune, i int) bool {
	if !unicode.IsUpper(runes[i]) {
		return false
	}
	if !unicode.IsUpper(runes[i-1]) {
		return true // lower → Upper
	}

	return i+1 < len(runes) && unicode.IsLower(runes[i+1]) // UPPER → Upper+lower
}

// ClassifyRole scores every role against the evidence and returns the winner
// with a 0..1 confidence. Pure function.
func ClassifyRole(f *Facts) (Role, float64) {
	sheet := scoreRoles(f)
	role := sheet.winner()

	return role, sheet.confidence(role)
}

// ramp is a clamped linear interpolation: 0 at or below lo, 1 at or above hi.
// It replaces the classifier's step thresholds so a thread just short of a cut
// still contributes proportional evidence instead of scoring nothing.
func ramp(value, lo, hi float64) float64 {
	if hi <= lo {
		return boolTo1(value >= hi)
	}

	return clamp01((value - lo) / (hi - lo))
}

func clamp01(v float64) float64 { return math.Min(1, math.Max(0, v)) }

// parseBucket maps a config bucket name to its Bucket; unknown names yield
// BucketNone, which force rules treat as "no override".
func parseBucket(name string) Bucket {
	switch name {
	case "critical":
		return BucketCritical
	case "interactive":
		return BucketInteractive
	case "background":
		return BucketBackground
	case "untouchable":
		return BucketUntouchable
	default:
		return BucketNone
	}
}

// overrideBucket resolves the bucket decisions that do not depend on the
// classifier at all: the user's config rules and the game's own priority
// intent. An exclude match makes a thread untouchable, a force rule pins its
// bucket outright, and otherwise the game's own elevated threads are
// untouchable — except vivoxsdk voice threads (user decision: voice chat may be
// tuned). These are observations rather than inferences, so the classifier
// commits them immediately instead of waiting out a stability streak.
//
// "The game's own" is load-bearing: f.TimeCritical is derived from the priority
// baseline captured before the governor touched anything. Reading the live
// priority instead made every thread the governor promoted to
// THREAD_PRIORITY_HIGHEST look game-elevated on the next tick, so the governor
// promoted threads straight into its own untouchable bucket and never tuned
// them again.
func overrideBucket(f *Facts) (Bucket, bool) {
	switch {
	case f.Excluded:
		return BucketUntouchable, true
	case f.ForcedBucket != BucketNone:
		return f.ForcedBucket, true
	case f.TimeCritical && !strings.Contains(f.Module, "vivoxsdk"):
		return BucketUntouchable, true
	}

	return BucketNone, false
}

// RoleBuckets is the role → bucket policy: which tuning treatment each
// classified role receives. Configurable per game through auto.role_buckets.
type RoleBuckets [roleCount]Bucket

// DefaultRoleBuckets is the policy used when the config says nothing.
//
// Audio and network sit below the frame-critical set on purpose. Both are
// latency-sensitive rather than throughput-sensitive: a mixer or a socket pump
// wakes often and does almost nothing per wake, so it gains nothing from a
// dedicated physical core and an elevated priority, while the simulation and
// render threads it was competing with lose one. Demoting them is a real
// trade-off — a netcode thread that wakes late delays every packet behind it —
// so both are one step down rather than at the bottom, and the whole table can
// be overridden per game.
func DefaultRoleBuckets() RoleBuckets {
	var buckets RoleBuckets

	buckets[RoleUnknown] = BucketNone
	buckets[RoleMainSim] = BucketCritical
	buckets[RoleRenderSubmit] = BucketCritical
	buckets[RoleGPUWorker] = BucketCritical
	buckets[RoleInput] = BucketCritical
	buckets[RoleAudio] = BucketInteractive
	buckets[RoleJobWorker] = BucketInteractive
	buckets[RoleLoader] = BucketInteractive
	buckets[RoleNetwork] = BucketBackground
	buckets[RolePoolIdle] = BucketBackground
	buckets[RoleTelemetry] = BucketBackground

	return buckets
}

// ParseRoleBuckets applies config overrides to the default policy, returning
// the resolved table and a message for every entry it could not use. Unknown
// role or bucket names are reported rather than ignored: a typo in a policy
// override is silent misconfiguration otherwise.
func ParseRoleBuckets(overrides map[string]string) (RoleBuckets, []string) {
	buckets := DefaultRoleBuckets()
	if len(overrides) == 0 {
		return buckets, nil
	}

	byName := make(map[string]Role, roleCount)
	for role, name := range roleNames {
		byName[name] = role
	}

	var problems []string
	for name, bucketName := range overrides {
		role, ok := byName[name]
		if !ok || role == RoleUnknown {
			problems = append(problems, fmt.Sprintf("role_buckets: no such role %q; ignored", name))
			continue
		}
		bucket := parseBucket(bucketName)
		if bucket == BucketNone {
			problems = append(problems, fmt.Sprintf("role_buckets[%s]: no such bucket %q; ignored", name, bucketName))
			continue
		}
		buckets[role] = bucket
	}
	sort.Strings(problems) // map iteration order must not reorder the warnings

	return buckets, problems
}

// Of maps a role to its tuning bucket, ignoring overrides.
func (p RoleBuckets) Of(role Role) Bucket {
	if int(role) >= roleCount {
		return BucketNone
	}

	return p[role]
}

// BucketFor is the full bucket decision for one window: overrides first, then
// the role mapping. See overrideBucket.
func (p RoleBuckets) BucketFor(role Role, f *Facts) Bucket {
	if bucket, ok := overrideBucket(f); ok {
		return bucket
	}

	return p.Of(role)
}

// bucketSafety orders buckets by how safe it is for a thread to sit in one.
// Moving to a safer bucket needs less evidence than moving to a more
// restrictive one: leaving a load-bearing thread demoted costs frames every
// frame, while leaving a junk thread untuned for a few more windows costs
// nothing at all.
func bucketSafety(b Bucket) int {
	switch b {
	case BucketBackground:
		return 0
	case BucketInteractive:
		return 1
	case BucketCritical:
		return 2
	default:
		return 3 // None / Untouchable — the governor stops interfering entirely
	}
}

func matchesAny(value string, substrings []string) bool {
	for _, substr := range substrings {
		if strings.Contains(value, substr) {
			return true
		}
	}
	return false
}

// Verdict is the hysteresis-filtered classification of one thread.
type Verdict struct {
	Role       Role
	Confidence float64
	Bucket     Bucket
	Stable     bool // the committed bucket is still what this window's evidence says
	Streak     int  // consecutive windows agreeing on the current candidate bucket
}

type roleState struct {
	stable    Role
	candidate Role
	streak    int

	// Bucket hysteresis is tracked separately from role hysteresis; see Observe.
	bucket          Bucket
	bucketCandidate Bucket
	bucketStreak    int
	hasBucket       bool
}

// Classifier applies K-window stability filtering on top of per-window
// scoring so a briefly-spiking thread doesn't flap between buckets.
type Classifier struct {
	stableWindows int
	saferFactor   float64
	buckets       RoleBuckets
	states        map[thread.Key]*roleState
}

func NewClassifier(gates config.Gates, buckets RoleBuckets) *Classifier {
	c := &Classifier{
		buckets: buckets,
		states:  make(map[thread.Key]*roleState),
	}
	c.Retune(gates)

	return c
}

// Retune adopts new hysteresis settings without discarding what has already
// been observed. Per-thread streaks are kept deliberately: a settings change
// should take effect on the next window, not restart every thread's evidence
// from nothing.
func (c *Classifier) Retune(gates config.Gates) {
	c.stableWindows = max(gates.StableWindows, 1)

	c.saferFactor = gates.SaferBucketFactor
	if c.saferFactor <= 0 || c.saferFactor > 1 {
		c.saferFactor = 1
	}
}

// Observe folds one window's scored role into the thread's hysteresis state.
//
// Role and bucket are filtered separately. The role streak drives what the UI
// reports; the bucket streak drives actuation, because the bucket is the only
// thing the actuator acts on. Filtering only the role meant a thread whose
// evidence alternated between two roles in the *same* bucket — audio vs render,
// job-worker vs loader — never held a streak long enough to be actuated, even
// though every window agreed on what to do with it.
func (c *Classifier) Observe(key thread.Key, f *Facts) Verdict {
	sheet := scoreRoles(f)
	role := sheet.winner()

	state, ok := c.states[key]
	if !ok {
		state = &roleState{stable: RoleUnknown, candidate: role}
		c.states[key] = state
	}

	if role == state.candidate {
		state.streak++
	} else {
		state.candidate = role
		state.streak = 1
	}

	if state.streak >= c.stableWindows {
		state.stable = state.candidate
	}

	bucket, forced := overrideBucket(f)
	if !forced {
		bucket = c.buckets.Of(role)
	}
	c.observeBucket(state, bucket, forced)

	return Verdict{
		Role: state.stable,
		// Rated for the role being reported, not for this window's winner.
		// The two disagree whenever hysteresis is holding an older verdict,
		// and the row then showed one role's name beside another's confidence.
		Confidence: sheet.confidence(state.stable),
		Bucket:     state.bucket,
		Stable:     state.hasBucket && state.bucket == state.bucketCandidate,
		Streak:     state.bucketStreak,
	}
}

// observeBucket folds one window's bucket into the bucket hysteresis state.
func (c *Classifier) observeBucket(state *roleState, bucket Bucket, forced bool) {
	// A config rule or the game's own priority is not an inference we might
	// revise next window, so it takes effect at once — waiting out a streak
	// before honouring "never touch this thread" is exactly backwards.
	if forced {
		state.bucket, state.bucketCandidate = bucket, bucket
		state.bucketStreak = c.stableWindows
		state.hasBucket = true
		return
	}

	if bucket == state.bucketCandidate {
		state.bucketStreak++
	} else {
		state.bucketCandidate = bucket
		state.bucketStreak = 1
	}

	if state.bucketStreak >= c.windowsToCommit(state.bucket, bucket) {
		state.bucket = bucket
		state.hasBucket = true
	}
}

// windowsToCommit is how long a bucket change must hold before it is enacted.
// Asymmetric on purpose: a move to a safer bucket commits in a fraction of the
// windows, so undoing a demotion that turned out to be wrong is cheaper than
// making one. See bucketSafety and gates.safer_bucket_factor.
func (c *Classifier) windowsToCommit(from, to Bucket) int {
	if bucketSafety(to) <= bucketSafety(from) {
		return c.stableWindows
	}

	return max(int(math.Ceil(float64(c.stableWindows)*c.saferFactor)), 1)
}

// Prune drops hysteresis state for threads no longer tracked.
func (c *Classifier) Prune(live map[thread.Key]*Series) {
	for key := range c.states {
		if _, ok := live[key]; !ok {
			delete(c.states, key)
		}
	}
}
