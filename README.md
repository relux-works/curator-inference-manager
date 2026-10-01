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
| Engine catalog refusal/status mutator | Walk non-test `.go` files independently of build tags; reject non-regular entries; resolve refusal factories/types with `go/types` in Darwin/Linux/Windows contexts (including explicit-file contexts for standalone `//go:build ignore` commands). Enforce the closed P1–P4 propagation subset and explicit file/function/ordinal test certificates. R4 is frozen as singleton equality: identical guarded syntax and object path equals a typed constant literal (or standard deep equality with a fully specified non-comparable composite); observations and conjunction witnesses refuse. The original gate remains. Execution roots bind source/test content and require green selected baselines; equivalence requires executed joint downstream evidence. See [guard contract](spec/refusal-evidence.md) for the unconditional-refusal classification, defensive-unreachable and explicit undecided review bounds. | `go run scripts/engine-catalog-mutants.go -inventory`; `go test ./scripts -run TestRefusal -count=1 -v`; materialize platform trees with `scripts/test-nondarwin-emu.sh --materialize <linux\|windows> <source-root> <output-dir>`; run `go run scripts/engine-catalog-mutants.go -start N -count N -workers 8 -linux-emu-root <linux-dir> -windows-emu-root <windows-dir> -out .temp/<task>/mutants` | Inventory JSON/stdout and diagnostic stderr; tables, overlays and direct test logs under `.temp/`. A production inventory refusal is failing evidence, never green coverage. |
| skill-agents-management catalog contract test | Verify manager-produced schema v1 catalogs with the pinned public `pkg/runtimecatalog` reader in an isolated nested module. | `(cd contracttest && go test ./...)` | Go module/test output goes to stdout/stderr; generated manager catalogs and fixture files are written under `t.TempDir()` and removed when the test exits. |
| ShellCheck | Lint the validation script and compatibility shims | `shellcheck scripts/test-nondarwin-emu.sh scripts/qwen-infra scripts/model-harness` | Findings go to stdout/stderr; no repository artifacts are generated. |
| `curator-engines` | Inspect or manage configured engine runtimes | `go build -o .temp/curator-engines ./cmd/curator-engines`, then `.temp/curator-engines check --config /path/to/engines.toml --engine NAME --json`, or `.temp/curator-engines status --json` (`ensure`, `release`, `stop` also available) | Binary at `.temp/curator-engines`; check reports typed failures and KV RAM; status is printed to stdout with separate `engine_availability`, `engine_availability_reason`, and broker state fields; wire details follow `spec/engines-protocol-v1.md`. |
| K0 evidence harness | Enumerate refusal sites from Go AST and run fourteen narrowing mutants against the production CLI tests | `go run scripts/check-k0-evidence.go -out .temp/k0-evidence` | AST inventory, mutant results and individual test logs under `.temp/k0-evidence`; source bytes are restored after each mutant. |
| Task Board CLI | Track implementation status, checklist, notes, and task-scoped outcomes | `task-board q --format compact 'get(TASK-ID) { status checklist outcomeResources }'`; `task-board m 'set_status(TASK-ID, status=development)'`; `task-board resource add TASK-ID <file>` | Task state and attached outcomes go to the authoritative board selected by `TASK_BOARD_DIR`. |
| GitHub CLI | File the curator-spec adoption issue for the manager catalog contract | `gh issue create --repo relux-works/curator-spec --title 'Adopt curator-engines engine catalog schema v1 in Decision 0020' --body-file <body-file>` | The issue is stored in curator-spec; body scratch files are kept under task-scoped `.temp/`. |
| Non-Darwin file-set emulator | Run Linux and Windows file-set validation on the current host | `./scripts/test-nondarwin-emu.sh` | Temporary trees under `${TMPDIR:-/tmp}` are removed at exit; commands and results are printed to stdout/stderr. For an isolated mutant, `./scripts/test-nondarwin-emu.sh --materialize linux <source-root> <output-dir>` materializes one verified file set and retains it at `<output-dir>`. Each tree retains the unprojected candidate under `.temp/canonical-source`; full-module census tests bind its Go bodies and module inputs to the executing tree before checking all platform contexts. |
| Python 3 standard library | Parse `go list -json` manifests and compare the target and emulated `GoFiles`/`TestGoFiles` sets | Invoked by `./scripts/test-nondarwin-emu.sh`; no separate project command | Comparison metadata and helper are created under the script's temporary directory and removed at exit. |

## How it fits

- Curator discovers `curator-engines` as an umbrella provider on a trust root, where it ships beside `curator` or is put by `curator provider install engines`; Curator itself stays engine-blind.
- Runtime bindings in the machine catalog `~/.curator/runtimes.toml` reference engine entries in `~/.curator/engines.toml` by name. Adding an engine is a widening entry that takes effect after `curator engines confirm`. The agents-management module models engine kinds and lifecycle plans as values and never starts processes.
- Callers: task-board preflight calls `ensure` for engine-bound runtimes (a dry run never ensures), and `curator-run` calls it before building the launch. The launch itself is built by the agents-management launch plane.
- Roadmap: milestone M5 in `wiki/roadmap/ecosystem-roadmap.md`.

## Read-only engine check

`check` requires an explicit configuration file and entry name. It reads schema v1 `[engines.NAME]` entries through the existing strict catalog reader, transcribing configured argv plus model and knobs; legacy `[agents.pi.profiles.NAME]` entries (`family = "llama.cpp"` or `"llama-cpp"`, `runtime.argv`) remain accepted. A catalog read/validation failure never falls back to a legacy entry. SSH entries remain outside the local llama-cpp rule. The offline transcription retains configured identity options for validation; it does not perform launch-time alias replacement. It never searches HOME, inherits `LLAMA_ARG_*`, or starts an engine. Relative model paths are resolved against the supplied configuration file's directory. JSON includes all typed failures and `kv_ram` (`known` with bytes, or `unknown` with a reason code). Exit 0 means no check failures; exit 1 means entry/check failures; exit 2 means CLI usage failure. An absent model alone is an unknown estimate, not a refusal. Malformed or unreadable metadata is a refusal.

The K0 argv subset accepts separated values only: `--parallel`/`-np`, `--kv-unified`/`-kvu`, `--no-kv-unified`/`-no-kvu`, `--cache-ram`/`-cram`, `--no-cache-idle-slots` (inverse refused), `--chat-template-file` or `--chat-template`, `--cache-type-k`/`-ctk`, `--cache-type-v`/`-ctv`, `--model`/`-m`, and `--ctx-size`/`-c`. Sleep-idle, router selectors, and template-disabling prefix/suffix flags are refused. Unknown argv tokens are refused as `argv_unsupported`; the exact pass-through flags are listed in `engineCheckFlags`. Value-taking options consume the next token verbatim, including option-looking data; a missing token is refused as `argv_missing_value`. Required value flags must occur once. Template pinning means an explicit argv selection; this offline check does not attest file contents or availability.

KV bytes estimate the K/V tensor payload, excluding allocator overhead, weights, compute buffers, prompt cache and device placement. GGUF v2/v3 headers are read without tensor descriptors or tensor data. Ordinary llama/qwen2/qwen3 attention metadata with scalar dimensions is supported; hybrid/recurrent, sliding-window and other architectures report unknown. f16 uses two bytes per element; q8_0 uses 34 bytes per 32 elements. Other valid cache types report unknown. Total context is rounded to 256 cells; non-unified layout divides that total by parallel and rounds each stream to 256 cells. Missing context or explicit zero uses GGUF training context. Context/cells are bounded to 2^40 and layer/head dimensions to 10000 before arithmetic; out-of-bound or overflowing estimates report `unknown` (`kv_size_overflow`). These rules match the fixture's llama.cpp revision `c21284cdf`; `check` reports the configured estimate, not live allocation or backend fitting.

## Legacy import

Preview the selected old engine profiles and terminal lifecycle envelopes with
`curator-engines import-legacy preview --config FILE --evidence-root DIR`.
Use `apply`, `resume`, or `rollback` with the same inputs. The importer never
starts an engine or retires the old evidence. See [field inventory, transaction
rules and compatibility commands](spec/legacy-engine-import.md).

| Tool | Use | Run | Outputs |
| --- | --- | --- | --- |
| Legacy importer | Preview, apply, resume or roll back selected engine/state inputs | `curator-engines import-legacy preview --config FILE --evidence-root DIR`; replace `preview` with `apply`, `resume` or `rollback` | JSON includes per-field `field_plan` actions; absent fields preserve imported values. Private catalog and summaries under `~/.curator/`; selected backups and frozen journal under `~/.curator/.legacy-import/`; preview output is JSON only. |
| Legacy evidence reader | Inspect admitted terminal facts without retirement | `curator-engines legacy-evidence DIR` | Sanitized JSON on stdout; old evidence stays read-only. |
| Compatibility shims | Route explicit qwen/model-harness commands to the manager | `scripts/qwen-infra print-config --engine NAME`; `scripts/model-harness render NAME` | Manager JSON on stdout; engine operations use the existing broker/lease APIs. Install the shims alongside an already installed `curator-engines` binary. |
| Import refusal mutator | Select certified importer mutants after the complete typed module census | `go run scripts/engine-catalog-mutants.go -inventory`; `go run scripts/engine-catalog-mutants.go -files internal/infra/legacy_import.go,internal/infra/legacy_import_journal.go,internal/infra/legacy_import_runtime_state.go,internal/infra/legacy_import_profiles.go,internal/infra/pi_terminal_evidence.go,internal/infra/engine_compat_commands.go,internal/infra/pi_shared_resolve.go -task TASK-260929-28odl0 -start N -count 20 -workers 1 -linux-emu-root <linux-dir> -windows-emu-root <windows-dir> -out .temp/<task>/mutations` | Task-named tables, overlays and direct behavioral logs under `.temp/`; exact source/test certificates in `scripts/engine-catalog-sites.json`. |

The importer certificates follow the frozen singleton equality rules in
[refusal evidence](spec/refusal-evidence.md). Observed length, method, error,
pointer and conditional-arm domains without an accepted witness remain explicit
source-bound undecided rows. Historical mutation kills are not current proof.
Equivalence requires executed joint evidence; a timeout is invalid evidence.

Import reports include `checks` per selected profile, using the same catalog
argv transcription and K0 validation as `curator-engines check`. Legacy llama.cpp
profiles keep their original values; missing K0 knobs are reported in preview.
KV RAM remains unknown when the model cannot be read.

Structured runtime state is validated before import admission. Restart ledgers
use the existing owner schema, exact JSON keys and terminal-state checks;
broker records and unknown state remain refused. Only layout-declared broker
and runtime logs stay opaque. Pending retries, half-open attempts, open restart
windows and readiness without terminal evidence refuse import admission.
