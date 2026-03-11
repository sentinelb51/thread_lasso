# GitHub Copilot Instructions

## Project overview
- `thread_lasso` is a Windows-only Go utility for detecting configured game processes and applying process/thread tuning.
- The JSON config is the source of truth. It describes per-game process settings and per-thread overrides.
- Current focus: process priority, process affinity, thread priority, thread affinity, and future thread matching by DLL/module name, stack usage, and related signals.

## Config contract
- `config.json` maps executable names to a `Game` configuration.
- `Game` supports:
  - `priority`
  - `io_priority`
  - `gpu_priority`
  - `affinity` as a zero-based list of logical CPU indexes
  - `cpu_sets`
  - `threads`
- `Thread` supports:
  - `name` for wildcard/pattern matching
  - `priority`
  - `io_priority`
  - `affinity` as a zero-based list of logical CPU indexes
  - `cpu_sets`
- Example: `"affinity": [0, 1, 2, 3, 4, 5, 6, 7]` means the first 8 logical CPUs.

## Implementation guidance
- Keep changes small and consistent with the existing package layout:
  - `config/` for config schema and loading
  - `scanner/` for process discovery
  - `process/` for process-level tuning helpers
  - `thread/` for thread-level helpers
  - `governor/` for orchestration logic if/when added
- Prefer reusable helpers that validate config input before calling Windows APIs.
- When converting affinity lists to masks, treat indexes as zero-based logical CPU IDs.
- Reject invalid affinity input early: empty lists, duplicates, negative values, or indexes beyond the supported affinity mask width.
- Preserve current JSON field names and public Go types unless the task explicitly requires a schema change.

## Roadmap assumptions
- Thread selection may expand beyond thread name matching to include DLL/module names (for example `vivoxsdk` or `binkasy*`) and stack-derived heuristics.
- If a feature is not implemented yet, document it clearly rather than pretending it already exists.
- Favor incremental Windows-safe building blocks over large framework-style rewrites.

## Quality bar
- Run `gofmt` on changed Go files.
- Run `go test ./...` after substantive Go changes.
- Add focused tests for config validation and affinity-mask behavior when public behavior changes.

