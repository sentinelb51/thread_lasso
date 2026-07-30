# AGENTS.md — thread_lasso

## Module identity
Go module name is `ThreadOrchestra` (not `thread_lasso`). Use this in all import paths:
```go
import "ThreadOrchestra/util"
```

## Architecture overview
```
config.json  ──load──▶  config/      (Config, Game, Thread, Auto structs)
                              │
                              ▼
                        scanner/     (blocking poll; returns first matched ps.Process)
                              │
                              ▼
                        process/     (stateless Win32 primitives: affinity, CPU sets,
                                      thread snapshot, topology, module resolution)
                        thread/      (per-thread stateful layer: handle cache, apply, journal)
                        governor/    (sample → metrics → classify → actuate loop + view-model)
                        ui/          (Fyne live view; default, dropped by the "nogui" build tag)
                        util/        (bitmask ↔ core-index conversions, glob matcher)
```
`main.go` loads config, scans for a game, then runs one `governor.Governor` per
session (reverting all thread changes on exit). Layering: `governor → process,
thread, config, util`; `thread → process, config, util`; `ui → governor`. No cycles.

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

### Config loading
`config.Load()` reads `config.json` from the **working directory**. The file must be present beside the binary at runtime.

### scanner.Process is blocking
`scanner.Process(cfg)` loops indefinitely at 1-second intervals until a configured executable is found. Do not call it in a goroutine you expect to time out without a context/cancel mechanism.

## Developer workflows
```powershell
# Build/run (Windows only — uses Windows APIs). The Fyne GUI is the default, so
# the build needs CGO and a C toolchain (MSYS2 / mingw-w64 gcc on PATH).
go build ./...
go run  .

# CGO-free build (CI / no C toolchain): the "nogui" tag drops Fyne; the tool
# then always uses the text reporter.
go build -tags nogui ./...
go test  -tags nogui ./...

# Run tests (compiles the GUI; needs CGO)
go test ./...

# Format before committing
gofmt -w .

# Run the tool, text mode (config.json must be present; admin needed for some
# priority/I-O APIs in full mode)
go run . -nogui
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
| `util/bitmask.go` | All core-index ↔ mask/CPU-set-ID math |
| `process/dll.go` | Single source of truth for Win32 proc handles |
| `process/cpusets.go` | Two-call pattern required by `GetProcessDefaultCpuSets` |
| `process/ntsnapshot.go` | System-wide thread snapshot (`NtQuerySystemInformation`) |
| `process/topology.go` | CPU-set base validation + SMT-aware physical-core leads |
| `thread/handles.go` / `apply.go` / `journal.go` | Handle cache, per-thread apply, revert journal |
| `governor/classify.go` | Graded role scoring + bucket policy + hysteresis |
| `governor/actuate.go` | Bucket × aggression × capability → thread changes + watchdog |
| `ui/fyne.go` | Fyne live table (default build; dropped by `-tags nogui`) |
| `config.json` | Live example of the full config schema |

