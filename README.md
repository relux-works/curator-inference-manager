# curator-inference-manager

The Curator inference plane. `curator-inference-manager` (renamed from curator-engine on 2026-09-24) is an umbrella provider that owns local inference engines for coding agents: llama.cpp, mlx-lm and mlx-swift. Its binary is `curator-engines`, so people and tools reach it as `curator engines …`: `list|show|add|remove|check|bench|confirm` for the catalog and `status|ensure|release|stop` for the engines (the command-line shape adopted on 2026-09-27). It holds a machine-local engine catalog, reports engine availability separately from known broker presence, grants leases, and runs engine lifecycle under host arbitration. It is carved out of the engine broker in the deprecated agents-infra residual.

The inference-plane design remains a draft; curator-engines v0.x has adopted the engine catalog schema v1 and supervision mapping in [`spec/inference-plane.md`](spec/inference-plane.md). Decision 0020 is pending in curator-spec issue [#104](https://github.com/relux-works/curator-spec/issues/104); the schema adoption proposal is tracked in [#114](https://github.com/relux-works/curator-spec/issues/114). The extracted broker wire contract is normative in [`spec/engines-protocol-v1.md`](spec/engines-protocol-v1.md); that v1 extraction preserves the versioned `pi_shared` wire fields, exit codes, and refusal codes while adding engine identity and process-lifetime lease fields.

## Layout

| Path | Content |
| --- | --- |
| `spec/inference-plane.md` | draft specification, extracted from the launch-profiles draft and the ecosystem roadmap |
| `spec/inference-plane.ru.md` | Russian translation of the draft specification (the English file is canonical) |

## Tools

| Tool | Use | Run | Outputs |
| --- | --- | --- | --- |
| Go toolchain | Build and validate the broker, CLI, and alias-list fake runtime | `go build ./...`; `go vet ./...`; `go test ./...`; `go test ./cmd/curator-engines/testdata/fake-runtime` | Build output stays in Go's cache; package results go to stdout/stderr. |
| `gofmt` | Format changed Go source and tests | `gofmt -w <changed .go files>` | Formatting updates the selected files in the worktree. |
| `golangci-lint` | Lint Go source on the configured host platform | `golangci-lint run` | Findings go to stdout/stderr; no repository artifacts are generated. |
| Engine catalog refusal/status mutator | Walk every non-test `.go` file from the module root regardless of build tags; skip only `testdata/`, `vendor/`, and dot-directories; refuse any symlink encountered; inventory typed refusals as direct return expressions or returning if-inits. Other shapes are out of scope, see TASK-260930-1hzscs. | `go run scripts/engine-catalog-mutants.go -inventory`; materialize Linux and Windows file sets with `scripts/test-nondarwin-emu.sh --materialize <linux|windows> <source-root> <output-dir>`; run bounded ranges with `go run scripts/engine-catalog-mutants.go -start N -count N -workers 8 -linux-emu-root <linux-dir> -windows-emu-root <windows-dir> -out <task-scoped-temp-dir-outside-worktree>` | Inventory JSON is printed or redirected to `.temp/`; tables, per-mutant source overlays, and direct `go test` logs go under `-out`. Darwin mutants run on the host; Linux/Windows platform-specific files use the verified emulated file sets. |
| skill-agents-management catalog contract test | Verify manager-produced schema v1 catalogs with the pinned public `pkg/runtimecatalog` reader in an isolated nested module. | `(cd contracttest && go test ./...)` | Go module/test output goes to stdout/stderr; generated manager catalogs and fixture files are written under `t.TempDir()` and removed when the test exits. |
| ShellCheck | Lint the non-Darwin validation script | `shellcheck scripts/test-nondarwin-emu.sh` | Findings go to stdout/stderr; no repository artifacts are generated. |
| `curator-engines` | Inspect or manage configured engine runtimes | `go build -o .temp/curator-engines ./cmd/curator-engines`, then `.temp/curator-engines status --json` (or `ensure`, `release`, `stop`) | Binary at `.temp/curator-engines`; status is printed to stdout with separate `engine_availability`, `engine_availability_reason`, and broker state fields; wire details follow `spec/engines-protocol-v1.md`. |
| Task Board CLI | Track implementation status, checklist, notes, and task-scoped outcomes | `task-board q --format compact 'get(TASK-ID) { status checklist outcomeResources }'`; `task-board m 'set_status(TASK-ID, status=development)'`; `task-board resource add TASK-ID <file>` | Task state and attached outcomes go to the authoritative board selected by `TASK_BOARD_DIR`. |
| GitHub CLI | File the curator-spec adoption issue for the manager catalog contract | `gh issue create --repo relux-works/curator-spec --title 'Adopt curator-engines engine catalog schema v1 in Decision 0020' --body-file <body-file>` | The issue is stored in curator-spec; body scratch files are kept under task-scoped `.temp/`. |
| Non-Darwin file-set emulator | Run Linux and Windows file-set validation on the current host | `./scripts/test-nondarwin-emu.sh` | Temporary trees under `${TMPDIR:-/tmp}` are removed at exit; commands and results are printed to stdout/stderr. For an isolated mutant, `./scripts/test-nondarwin-emu.sh --materialize linux <source-root> <output-dir>` materializes one verified file set and retains it at `<output-dir>`. |
| Python 3 standard library | Parse `go list -json` manifests and compare the target and emulated `GoFiles`/`TestGoFiles` sets | Invoked by `./scripts/test-nondarwin-emu.sh`; no separate project command | Comparison metadata and helper are created under the script's temporary directory and removed at exit. |

## How it fits

- Curator discovers `curator-engines` as an umbrella provider on a trust root, where it ships beside `curator` or is put by `curator provider install engines`; Curator itself stays engine-blind.
- Runtime bindings in the machine catalog `~/.curator/runtimes.toml` reference engine entries in `~/.curator/engines.toml` by name. Adding an engine is a widening entry that takes effect after `curator engines confirm`. The agents-management module models engine kinds and lifecycle plans as values and never starts processes.
- Callers: task-board preflight calls `ensure` for engine-bound runtimes (a dry run never ensures), and `curator-run` calls it before building the launch. The launch itself is built by the agents-management launch plane.
- Roadmap: milestone M5 in `wiki/roadmap/ecosystem-roadmap.md`.
