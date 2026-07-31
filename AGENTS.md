# AGENTS.md — thread_lasso

## Module identity
Go module name is `ThreadOrchestra` (not `thread_lasso`). Use this in all import paths:
```go
import "ThreadOrchestra/util"
```

## Architecture overview
```
config.json  ◀─save──▶  config/      (Config, Game, Thread, Auto structs;
                              │        Tuning + the reflection settings registry)
                              ▼
                        scanner/     (blocking poll; returns first matched ps.Process)
                              │
                              ▼
                        process/     (stateless Win32 primitives: affinity, CPU sets,
                                      thread snapshot, topology, module resolution,
                                      process-memory inspection)
                        thread/      (per-thread stateful layer: handle cache, apply, journal)
                        governor/    (sample → metrics → classify → actuate loop + view-model)
                        ui/          (Fyne live view + settings editor; default,
                                      dropped by the "nogui" build tag)
                        util/        (bitmask ↔ core-index conversions, glob matcher)
```
`main.go` owns one supervisor goroutine running the scan → attach → run →
rescan cycle for the life of the process, publishing a `ui.Event` per
transition. Both front ends consume that one stream: the Fyne window swaps
between a waiting screen and the dashboard, and `-nogui` prints the status and
runs `governor.Report` for the duration of each session. Layering:
`governor → process, thread, config, util`; `thread → process, config, util`;
`ui → governor`; `main → everything`. No cycles.

## Critical conventions

### CPU index representation
All CPU indices are **zero-based logical CPU IDs** throughout config and Go code. Two separate representations exist:
- **Affinity bitmask**: `util.CoreArrayToBitmask` / `BitmaskToCoreArray` — bit N set = core N active.
- **CPU Set IDs**: Windows uses `base + logicalIndex`; `util.LogicalToCpuSetIDs` / `CPUSetIDsToLogical` handle conversion. `base` defaults to `0x100` (`util.DefaultCpuSetBase`) and is validated/corrected at governor startup from `GetSystemCpuSetInformation` via `util.SetCpuSetBase` — see `process/topology.go`.

### Single processor group assumed
The tool targets gaming machines (≤ 64 logical CPUs, one processor group). CPU-set IDs, affinity masks (64-bit), and `SetThreadIdealProcessorEx` all hardcode group 0. Multi-group (>64 CPU) systems are out of scope.

### Windows API pattern
All Win32 proc variables live in `process/dll.go` as package-level `LazyProc` values:
```go
var (
    kernel32                   = windows.NewLazySystemDLL("kernel32.dll")
    procSetProcessAffinityMask = kernel32.NewProc("SetProcessAffinityMask")
)
```
New Windows APIs must follow this pattern — add the proc var to `dll.go`, implement the wrapper in a new or existing file in `process/` or `thread/`.

### Typed priorities
- `Game.Priority` / `Game.IOPriority` / `Game.GPUPriority` are **strings** (e.g. `"high"`, `"normal"`).
- `Thread.Priority` and `Thread.IOPriority` are **`*int`** — pointer is required because `0` is a meaningful value (`THREAD_PRIORITY_NORMAL`). Treat `nil` as "not configured".

### Never classify from a live thread priority
Classification reads `Series.BaselineRelative` — the thread's priority relative
to the process base, captured at its **first sighting**, before the governor
could write anything. The live value is unusable: `promotion_ceiling` defaults
to `+2` (`THREAD_PRIORITY_HIGHEST`), and in a HIGH-class process (Overwatch runs
at base 13) that lands on base priority 15 — exactly where a game-set
`THREAD_PRIORITY_TIME_CRITICAL` thread sits. Windows offers no way to tell the
two apart. Reading the live value made the governor promote a thread, read its
own write back as the game's intent, and move the thread to `BucketUntouchable`
— permanently locking itself out of every thread it tuned.

### Hysteresis filters the bucket, not just the role
`Classifier.Observe` keeps two streaks. The role streak drives what the UI
reports; the **bucket** streak drives actuation, because `Actuator.Apply` only
ever reads `Verdict.Bucket`. Filtering the role alone meant a thread whose
evidence alternated between two roles in the same bucket (audio vs render,
job-worker vs loader) reset its streak every window and was never actuated.
Two asymmetries are deliberate: `overrideBucket` results (exclude/force/game
Time Critical) commit on the first window because they are observations rather
than inferences, and a move to a *safer* bucket (`bucketSafety`) commits in half
`stable_windows` — undoing a demotion must be cheaper than making one.

### Wait reasons come in pairs, and WrQueue is not the only pool primitive
`DelayExecution` (4) and `WrDelayExecution` (11) both mean "the thread called
Sleep"; use `Series.WaitShareAny`. For thread pools, `WrQueue` (KQUEUE) is the
classic wait but modern engines park on `WrAlertByThreadId` (36) —
`NtWaitForAlertByThreadId`, the futex behind `WaitOnAddress`/SRWLOCK/condition
variables. Overwatch produces **zero** `WrQueue` waits, so a `WrQueue`-only rule
leaves the whole job system unclassified.

### Start addresses have four sources, and `governor.Identifier` owns all of them
Never read `Win32StartAddress` directly. `ETHREAD.Win32StartAddress` is writable
by the owning process through
`NtSetInformationThread(ThreadQuerySetWin32StartAddress)`, and Overwatch's
Eidolon protection clears it for every thread. On that process the kernel's own
`SYSTEM_THREAD_INFORMATION.StartAddress` reads zero as well, so the snapshot
alone recovers nothing. `Identifier.recoverEntry` tries four routes in cost
order, each existing because the one before it can be defeated:

1. `Win32StartAddress` from the snapshot — free, and user-writable;
2. the kernel's `StartAddress` from the same snapshot — free, not settable from
   user mode, but the information class is filtered in its own right;
3. `process.ThreadStartAddress` — `NtQueryInformationThread` against a handle we
   already hold, a different kernel path with different filtering;
4. `Inspector.Trace` — the thread's own startup stack frames. The routine
   `RtlUserThreadStart` was handed is still spilled near `StackBase`, and
   nothing can go back and unwrite the arguments a thread was created with.

A recovered address of 0 means every route failed; it must be treated as
"unknown" and never grouped on, because every thread would share it.
`ThreadSnapshot.EntryPoint()` still covers routes 1–2 for callers without an
Identifier (the probe, tests, limited mode).

### Stack fingerprints answer a different question than entry points
`Inspector.Trace` sweeps `[StackLimit, StackBase)` for pointer-aligned values
that land inside a known module. It is not an unwind — that needs unwind tables
— but where a thread *started* is a fact about how it was created, while what is
on its stack is a fact about what it does, and for a pool worker handed graphics
or socket work only the second is useful. Stale frames below the current one
count deliberately: nothing rewrites them, so a parked thread still shows the
subsystem it was last inside, which is what makes the signal usable at a 1.5s
poll. Sweeps are budgeted (`stackScansPerTick`) and refreshed on
`stackInterval`, so a full pass spans several ticks — anything that reports on
identification must wait for `entryCheckTick`.

### The module table is not just the loader list
`process.loadModuleTable` merges `EnumProcessModulesEx` with a `VirtualQueryEx`
sweep for `MEM_IMAGE` regions named through `GetMappedFileName`, because a
protection that unlinks or manually maps its modules is invisible to the PEB
loader list. `governor.Identifier` reloads the table (throttled to 15s, capped
at 8 attempts) while addresses are still failing to resolve — a protected
process finishes mapping long after a scanner first sees it. A reload never
discards an already-recovered address: an unnameable entry point still groups a
worker pool into a cohort.

### Bucket policy is data, not a switch statement
`DefaultRoleBuckets` is the role → bucket table and `auto.role_buckets`
overrides it per game. Audio sits in `interactive` and network in `background`
rather than the frame-critical set: both are latency-sensitive but not
throughput-sensitive, so a dedicated physical core buys them nothing and costs
the simulation one. This is a real trade-off — a netcode thread that wakes late
delays every packet behind it — which is why the table is configurable and why
`ParseRoleBuckets` reports unusable entries instead of dropping them silently.

### Every threshold lives in `config.Tuning`, and nothing may hide in a const
`config/tuning.go` holds all 145 knobs: what each bucket does
(`BucketActions`), per-role overrides of it (`RoleActions`), the gates between a
verdict and a write (`Gates`), the classifier's thresholds (`Signals`), and the
stack-scan budgets (`Scan`). A new tuned number goes there with a `desc` tag, not
into a `const` block — `config/settings.go` walks the struct by reflection to
build the settings UI, the `-settings` reference and the post-load range check
from one source, so an undocumented knob is one nobody can find or validate.
`TestEverySettingIsDescribedAndGrouped` enforces the tag.

Three things follow from the schema being fully populated rather than sparse:

- `Auto.UnmarshalJSON` decodes the file *over* `DefaultTuning(aggression)`, so an
  absent key means "keep the default" while an explicit zero means zero.
  `"eco_qos": false` and `"priority": 0` are both real settings; a
  pointer-and-omitempty scheme would erase them. Role overrides are the one
  sparse part, because "inherit" is a genuine third state.
- **Aggression is a preset, not a gate.** It selects the defaults for the whole
  table; it does not add conditions the actuator checks. `TestPresetsMatch...`
  pins each level to the behaviour it had when it was a switch statement.
- `Setting.Reset` restores the *preset*, not the zero value, and deep-copies —
  aliasing the defaults would let one edit rewrite the table it is compared to.

The role names are spelled in both packages (`governor.roleNames` and
`config.RoleActions` json tags) because config cannot import governor without a
cycle. `TestRoleNamesAgreeAcrossPackages` is what keeps them honest.

### Settings changes must undo before they redo
`Governor.ApplyTuning` bumps a generation counter alongside the swapped table.
`Actuator.applyOne` treats a generation change exactly like a bucket change — it
restores the thread from the journal before re-tuning — because the settings
that are on a thread were written under rules that no longer exist. The one
asymmetry: a bucket move costs a cooldown before re-tuning (a flapping
classification must not drive a burst of writes) while a settings edit does not,
since someone is watching the window and a 30-second delay reads as the control
being broken.

### "Demoted" means held back by any means, not lowered priority
`actionState.lowered` is set from `BucketAction.Lowers()`, which is true for
memory priority, I/O throttling, EcoQoS and a background core set as well as a
negative priority. Now that any bucket can be configured to lower things, keying
the starvation watchdog on `BucketBackground` would leave a lowered `interactive`
thread unprotected. A thread throttled into a stall is starved however it got
there.

`checkPolicy` warns when an edit lowers a bucket whose roles are not in
`gates.demote_roles` — the two settings live in different sections and the
combination is silently inert. It compares against the preset and stays quiet
about the shipped defaults, which contain that combination deliberately:
`standard` puts network threads in a lowering bucket while declining to demote
them, and that *is* how "demote only what I am sure about" is expressed.

### ui/ui.go must not import Fyne
The `ui` package is compiled in `-tags nogui` CGO-free builds too; only
`fyne.go` carries the `!nogui` tag. Layout, grouping, formatting and the colour
palette live in `ui.go` behind Fyne-free stand-ins (`textAlign`, `cell` with a
`color.Color`), and `fyne.go` maps them. Putting a `fyne.io/...` import in
`ui.go` breaks CI.

### Notability is behavioural, not nominal
`ui.notable` decides which threads survive the default filter. It must not key
off `ThreadRow.Module`: that test dates from when almost nothing resolved, and
now that `EntryPoint()` survives a scrubbed `Win32StartAddress` every thread has
a module, so the filter would pass the entire idle pool. Use bucket, starvation,
applied actions, a game-set description, or measured cycles.

### Config loading
`config.Load()` reads `config.json` from the **working directory**, and
`config.EnsureFile()` writes a starter one (a placeholder game in `observe`
mode, every tuning key at its default) when it is absent, so a first run has
something to read and to learn from. `Load` returns problems alongside the
config rather than failing: a typo in one threshold is reported and reset, not a
reason the tool will not start.

`config.Save` is what the settings panel writes through, so **round-tripping is
this package's most important property** — anything `Load` reads and `Save` does
not write is destroyed the first time someone changes a number in the UI.
`TestSaveKeepsEverythingLoadRead` covers manual thread rules, exclude/force
rules, `role_buckets` and the other games in the file. Superseded keys
(`promotion_ceiling`, `demotion_floor`, `poll_interval_ms`, `stable_windows`,
`cooldown_ms`) migrate into `tuning` on load, are reported, and are cleared so
the next save writes one form only.

### scanner.Process is blocking
`scanner.Process(ctx, cfg)` loops at 1-second intervals until a configured
executable is found or `ctx` is cancelled. The context is what lets the GUI
close its window while nothing has been found yet; `main` then waits up to
`shutdownGrace` for the supervisor to unwind so the governor's revert runs.

## Developer workflows
```powershell
# Build/run (Windows only — uses Windows APIs). The Fyne GUI is the default, so
# the build needs CGO and a C toolchain. Install one once:
#   winget install --id BrechtSanders.WinLibs.POSIX.UCRT --scope user
#   go env -w CGO_ENABLED=1
# winget puts the mingw64 bin on the user PATH itself. First build is ~3½
# minutes (glfw); after that the cache makes vet and test ~2 seconds.
go build ./...
go run  .

# CGO-free build: the "nogui" tag drops Fyne and the tool always uses the text
# reporter. Keep it working — CI builds it, and it is the fallback when no C
# toolchain is available.
go build -tags nogui ./...
go test  -tags nogui ./...

# Run tests (compiles the GUI; needs CGO). The Fyne panels are covered
# headlessly through fyne.io/fyne/v2/test, so `ui` is real test coverage now,
# not just a compile check.
go test ./...

# Format before committing
gofmt -w .

# Run the tool, text mode (config.json must be present; admin needed for some
# priority/I-O APIs in full mode)
go run . -nogui

# Diagnose thread identification against a running game: dumps every raw field
# the four recovery routes read, side by side, and exits. Attaches and tunes
# nothing. This is the first thing to run when threads show up unidentified.
go run . -probe

# Print the tuning reference: every key, its default under that preset, and the
# sentence describing it. Same registry the Settings tab renders, so the two
# cannot drift.
go run . -settings aggressive
```

## Unimplemented areas (document, don't pretend they exist)
- `thread/threads.go` — empty placeholder; the real per-thread layer is `thread/handles.go` (cache), `thread/apply.go` (apply), `thread/journal.go` (revert).
- **TEB stack read** (exact per-thread stack usage via `ReadProcessMemory`) — full-mode extra, not implemented.
- **Recorded fixtures / `-record` flag** — the classifier is unit-tested with synthetic streams (`governor/classify_test.go`); JSON fixtures seeded from the System Informer captures are a roadmap item.
- **Live validation** against Overwatch in observe mode is pending (see the plan).

## Key files
| Path | Purpose |
|---|---|
| `config/structs.go` | Canonical schema — all JSON field names are frozen |
| `config/tuning.go` | Every tuned number and switch, with its description and range |
| `config/settings.go` | Reflection registry: one source for the UI, the reference and validation |
| `util/bitmask.go` | All core-index ↔ mask/CPU-set-ID math |
| `process/dll.go` | Single source of truth for Win32 proc handles |
| `process/cpusets.go` | Two-call pattern required by `GetProcessDefaultCpuSets` |
| `process/ntsnapshot.go` | System-wide thread snapshot (`NtQuerySystemInformation`) |
| `process/topology.go` | CPU-set base validation + SMT-aware physical-core leads |
| `thread/handles.go` / `apply.go` / `journal.go` | Handle cache, per-thread apply, revert journal |
| `governor/classify.go` | Graded role scoring + bucket policy + hysteresis |
| `governor/actuate.go` | Bucket × aggression × capability → thread changes + watchdog |
| `governor/view.go` | `ThreadRow`/`ViewModel` + the `Identity()` fallback chain |
| `governor/identify.go` | Per-thread identity cache: four start-address routes + stack sweeps |
| `process/inspect.go` | Process-memory reads: stack sweep, TEB flag, module lookup |
| `process/modules.go` | Loader list + mapped-image sweep behind one address lookup |
| `probe.go` | `-probe`: raw per-thread identity fields, side by side |
| `ui/ui.go` | Fyne-free layout, grouping, palette (compiles CGO-free) |
| `ui/fyne.go` | Fyne window: waiting screen + Threads/Settings tabs (dropped by `-tags nogui`) |
| `ui/settings.go` | Settings tab: editors, per-setting reset, apply/save |
| `config.json` | The user's live config; `go run . -settings` is the schema reference |

