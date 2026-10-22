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
| Darwin code identity verification | Compare a built Mach-O CodeDirectory with the platform tool and exercise CS_VALID, kernel cdhash, fail-closed, post-authentication exec and pathname replacement checks | `go test ./internal/infra -run '^TestSharedCode' -count=1`; the tests invoke `codesign -dvvv` when available | Fixtures use `t.TempDir()`; test output goes to stdout/stderr. Socketpair exec/recheck tests need no bind; filesystem socket integration requires permission to bind local Unix sockets. |
| `gofmt` | Format changed Go source and tests | `gofmt -w <changed .go files>` | Formatting updates the selected files in the worktree. |
| `golangci-lint` | Lint Go source on the configured host platform | `golangci-lint run` | Findings go to stdout/stderr; no repository artifacts are generated. |
| Engine catalog refusal/status mutator | Walk non-test `.go` files independently of build tags; reject non-regular entries; resolve refusal factories/types with `go/types` in Darwin/Linux/Windows contexts (including explicit-file contexts for standalone `//go:build ignore` commands). Enforce the closed P1–P4 propagation subset and explicit file/function/ordinal test certificates. R4 is frozen as singleton equality: identical guarded syntax and object path equals a typed constant literal (or standard deep equality with a fully specified non-comparable composite); observations and conjunction witnesses refuse. The original gate remains. Execution roots bind source/test content and require green selected baselines; equivalence requires executed joint downstream evidence. See [guard contract](spec/refusal-evidence.md) for the unconditional-refusal classification, defensive-unreachable and explicit undecided review bounds. | `go run scripts/engine-catalog-mutants.go -inventory`; `go test ./scripts -run TestRefusal -count=1 -v`; materialize platform trees with `scripts/test-nondarwin-emu.sh --materialize <linux\|windows> <source-root> <output-dir>`; run `go run scripts/engine-catalog-mutants.go -start N -count N -workers 8 -linux-emu-root <linux-dir> -windows-emu-root <windows-dir> -out .temp/<task>/mutants` | Inventory JSON/stdout and diagnostic stderr; tables, overlays and direct test logs under `.temp/`. A production inventory refusal is failing evidence, never green coverage. |
| skill-agents-management contract tests | Verify generated catalogs with the pinned public `pkg/runtimecatalog` reader and generated status documents with a local copy of the status consumer rules in an isolated nested module. | `(cd contracttest && go test ./...)` | Go module/test output goes to stdout/stderr; generated manager catalogs and fixture files are written under `t.TempDir()` and removed when the test exits. |
| ShellCheck | Lint the validation script and compatibility shims | `shellcheck scripts/test-nondarwin-emu.sh scripts/qwen-infra scripts/model-harness` | Findings go to stdout/stderr; no repository artifacts are generated. |
| `curator-engines` | Inspect or manage configured engine runtimes | `go build -o .temp/curator-engines ./cmd/curator-engines`, then `.temp/curator-engines check --config /path/to/engines.toml --engine NAME --json`, or `.temp/curator-engines status --json` (`ensure`, `release`, `stop`, `quarantine`, `unquarantine` also available); runtime commands accept `[--config FILE] [--cache-root DIR]`; `check` also accepts `--cache-root DIR` | Binary at `.temp/curator-engines`; catalogs use mode `0600` and integer `knobs.kv_context_size`; optional `[engines.NAME.weights]` pins exact GGUF bytes. Compute with `.temp/curator-engines weights compute --config FILE --engine NAME [--json]`; ensure accepts `--expected-weights-id ID`; check reports typed failures, weights verification and KV RAM; status is printed to stdout with separate `engine_availability`, `engine_availability_reason`, and broker state fields; wire details follow `spec/engines-protocol-v1.1.md`. |
| K0 evidence harness | Enumerate refusal sites from Go AST and run fourteen narrowing mutants against the production CLI tests | `go run scripts/check-k0-evidence.go -out .temp/k0-evidence` | AST inventory, mutant results and individual test logs under `.temp/k0-evidence`; source bytes are restored after each mutant. |
| Task Board CLI | Track implementation status, checklist, notes, and task-scoped outcomes | `task-board q --format compact 'get(TASK-ID) { status checklist outcomeResources }'`; `task-board m 'set_status(TASK-ID, status=development)'`; `task-board resource add TASK-ID <file>` | Task state and attached outcomes go to the authoritative board selected by `TASK_BOARD_DIR`. |
| GitHub CLI | File the curator-spec adoption issue for the manager catalog contract | `gh issue create --repo relux-works/curator-spec --title 'Adopt curator-engines engine catalog schema v1 in Decision 0020' --body-file <body-file>` | The issue is stored in curator-spec; body scratch files are kept under task-scoped `.temp/`. |
| Non-Darwin file-set emulator | Run Linux and Windows file-set validation on the current host | `./scripts/test-nondarwin-emu.sh` | Temporary trees under `${TMPDIR:-/tmp}` are removed at exit; commands and results are printed to stdout/stderr. For an isolated mutant, `./scripts/test-nondarwin-emu.sh --materialize linux <source-root> <output-dir>` materializes one verified file set and retains it at `<output-dir>`. Each tree retains the unprojected candidate under `.temp/canonical-source`; full-module census tests bind its Go bodies and module inputs to the executing tree before checking all platform contexts. |
| Python 3 standard library | Parse `go list -json` manifests and compare the target and emulated `GoFiles`/`TestGoFiles` sets | Invoked by `./scripts/test-nondarwin-emu.sh`; no separate project command | Comparison metadata and helper are created under the script's temporary directory and removed at exit. |

`status`, `ensure`, `release` and `stop` accept `--config FILE` to select an
absolute catalog path and `--cache-root DIR` to select an existing absolute
state/cache directory for broker state, leases and the rendezvous socket.
Catalogs retain the private `0600` convention and strict reader. Relative config
paths refuse with `engine_catalog_read_failed`; relative cache roots refuse with
`shared_runtime_state_path_invalid`. An explicit missing catalog or entry refuses
instead of falling back to the default catalog or legacy configuration.

Catalog entries shut down immediately after the last lease is released.
Release acknowledgement can precede shutdown completion; a hello interrupted
by shutdown refuses with typed `shared_runtime_unavailable`, even with
`stop --force`. After shutdown completes, `stop --force` is a successful
`absent` no-op with both termination flags false. Non-forced stop refuses an
absent broker with `shared_runtime_broker_unreachable_use_force`, or active
leases with `shared_runtime_leases_active`.

Without these flags the catalog remains `$HOME/.curator/engines.toml` and state
remains under the user cache directory. The broker and runtime launcher receive
the same selection. The rendezvous socket path has a 103-byte limit after cache
root canonicalization. `check`, `ensure`, and `status` reject an excessive root before creating state, including a missing root. The root budget is computed from the existing socket suffix (currently 44 bytes); a long `--cache-root` refuses with
`broker_rendezvous_path_too_long`, reporting the actual length and the limit;
the socket is never relocated automatically.

Catalog location alone is excluded from runtime/engine identity. Pinned weights
with a relative model path resolve against the catalog directory; moving such a
catalog can change the expanded model path and therefore the runtime key.

`quarantine` and `unquarantine` accept `--engine NAME`, `--config FILE` and
`--cache-root DIR` with the same absolute-path selection as `status`. They emit
the restart ledger as JSON and refuse `shared_runtime_active` while the broker
lock is held. The existing compat aliases remain available; compat `--config`
continues to verify a legacy profile against the default catalog.

Before publishing status JSON, both the top-level and compat status paths check
the v1 consumer contract and v3 readings shape. Invalid reports refuse `status_contract_violation`
with no status bytes on stdout. Readings failures remain inside v3 facts as
`not-observed` with a `readings_*` reason code, so broker availability can still
be read independently. Unknown/refused broker record states use the permitted
`unverified-stale/record-derived-unverified` pair and a typed `broker.reason`.

## How it fits

- Curator discovers `curator-engines` as an umbrella provider on a trust root, where it ships beside `curator` or is put by `curator provider install engines`; Curator itself stays engine-blind.
- Runtime bindings in the machine catalog `~/.curator/runtimes.toml` reference engine entries in `~/.curator/engines.toml` by name. Adding an engine is a widening entry that takes effect after `curator engines confirm`. The agents-management module models engine kinds and lifecycle plans as values and never starts processes.
- Callers: task-board's engine lease path calls `ensure` for engine-bound runtimes and holds the process-lifetime lease (a dry run never ensures). skill-agents-management's `pkg/localruntime` reads `status --json` for availability and Pi preflight; its launch plane builds the agent launch.
- Roadmap: milestone M5 in `wiki/roadmap/ecosystem-roadmap.md`.

## Read-only engine check

`check` requires an explicit configuration file and entry name. It reads schema v1 `[engines.NAME]` entries through the existing strict catalog reader, transcribing configured argv plus model and knobs; legacy `[agents.pi.profiles.NAME]` entries (`family = "llama.cpp"` or `"llama-cpp"`, `runtime.argv`) remain accepted. A catalog read/validation failure never falls back to a legacy entry. SSH entries remain outside the local llama-cpp rule. The offline transcription retains configured identity options for validation; it does not perform launch-time alias replacement. It never searches HOME for engine configuration, inherits `LLAMA_ARG_*`, or starts an engine. Relative model paths are resolved against the supplied configuration file's directory. JSON includes all typed failures and `kv_ram` (`known` with bytes, or `unknown` with a reason code). Exit 0 means no check failures; exit 1 means entry/check failures; exit 2 means CLI usage failure. An absent model alone is an unknown estimate, not a refusal. Malformed or unreadable metadata is a refusal.

`knobs.reasoning = "on" | "off" | "auto"` is available only for local llama-cpp entries and renders `--reasoning VALUE`. Omission emits no flag. Invalid values or combining the knob with literal `--reasoning`/`-rea` argv refuses as `engine_catalog_malformed` at `entry.knobs.reasoning` or `entry.knobs.reasoning.argv`. The other knob keys remain `kv_context_size`, `prefill_chunk_size`, `reasoning_effort`, and `speculative_decoding`.

The K0 argv subset accepts separated values only: `--parallel`/`-np`, `--kv-unified`/`-kvu`, `--no-kv-unified`/`-no-kvu`, `--cache-ram`/`-cram`, `--no-cache-idle-slots` (inverse refused), `--chat-template-file` or `--chat-template`, `--cache-type-k`/`-ctk`, `--cache-type-v`/`-ctv`, `--model`/`-m`, and `--ctx-size`/`-c`. `--reasoning`/`-rea` and `--reasoning-budget` are accepted pass-through flags; `--chat-template-kwargs` remains refused. Sleep-idle, router selectors, and template-disabling prefix/suffix flags are refused. Unknown argv tokens are refused as `argv_unsupported`; the exact pass-through flags are listed in `engineCheckFlags`. Value-taking options consume the next token verbatim, including option-looking data; a missing token is refused as `argv_missing_value`. Required value flags must occur once. Template pinning means an explicit argv selection; this offline check does not attest file contents or availability.

KV bytes estimate the K/V tensor payload, excluding allocator overhead, weights, compute buffers, prompt cache and device placement. GGUF v2/v3 headers are read without tensor descriptors or tensor data. Ordinary llama/qwen2/qwen3 attention metadata with scalar dimensions is supported; hybrid/recurrent, sliding-window and other architectures report unknown. f16 uses two bytes per element; q8_0 uses 34 bytes per 32 elements. Other valid cache types report unknown. Total context is rounded to 256 cells; non-unified layout divides that total by parallel and rounds each stream to 256 cells. Missing context or explicit zero uses GGUF training context. Context/cells are bounded to 2^40 and layer/head dimensions to 10000 before arithmetic; out-of-bound or overflowing estimates report `unknown` (`kv_size_overflow`). These rules match the fixture's llama.cpp revision `c21284cdf`; `check` reports the configured estimate, not live allocation or backend fitting.

## Exact model weights

Catalogs are owner-private regular files, conventionally mode `0600` (group and
other access is refused). `knobs.kv_context_size` is a positive TOML integer for
llama-cpp, such as `kv_context_size = 16384`, never a quoted numeric string.
Other supported engines may use the documented `"unbounded"` value.

Local llama-cpp entries with one unsplit GGUF model file may declare:

```toml
[engines.local-model.weights]
id = "gguf-sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
```

The example digest must be replaced with the file's actual id. Compute it with
`curator-engines weights compute --config FILE --engine NAME [--json]`; this
streams SHA-256 over all file bytes and never edits the catalog. Relative model
paths resolve beside FILE. The table requires exactly `id`, matching
`^gguf-sha256:[0-9a-f]{64}$`; no whitespace, normalization or version member is
accepted. Other prefixes, engine kinds and SSH entries are unsupported.
The shared GGUF reader validates v2/v3 headers and metadata (bounded to 32 MiB)
through the same descriptor and bytes used for hashing. Non-GGUF bytes, unsupported
versions and truncated metadata refuse with `engine_weights_not_gguf`.
Metadata `split.count > 1` or any `split.no` refuses with
`engine_weights_split_unsupported`, regardless of the filename. These codes apply
to compute, pinned check and the ensure gate. Missing, unreadable and non-regular
files also refuse compute.
Legacy imports do not create an id; entries without weights remain unpinned.

`check` verifies a declared id when the model exists. A missing model remains
unknown. Weights catalog refusals preserve `code` and `site` in check JSON:
`engine_catalog_malformed` at `catalog.weights`, `catalog.weights.keys` or
`entry.weights.id`; `engine_catalog_unsupported` at
`entry.weights.id.unsupported` or `entry.weights.scope`. Other catalog validation
errors retain `engine_config_malformed` for compatibility.
`ensure --engine NAME --expected-weights-id ID` additionally requires
the operator pin, catalog id and actual id to agree; a pin cannot certify an
unpinned entry. Before launch the broker verifies the file using a private metadata-keyed digest cache (or hashes on a cache miss); after readiness it
rechecks device, inode, size and mtime and stops its process if they changed.
Shared ensures use that running instance's verified id without hashing again.
`status --json` adds `weights` with `id` when declared, `verification`
(`verified`, `mismatch`, `unknown`, `unpinned`), and, when verified,
`observed_id` and `observed_at`. Verification describes the observation at
launch, not continuous monitoring of the pathname. See the
[protocol](spec/engines-protocol-v1.1.md) for refusal codes and provenance.

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

### Status readings (protocol v1.1)

`go run ./cmd/curator-engines status --engine NAME --json` adds the 17-fact
`observed-process/v3` readings envelope. See
[protocol v1.1](spec/engines-protocol-v1.1.md) for provenance, typed failure and
unknown semantics. The normal required schema module is pinned to v0.5.39;
existing status and socket version numbers remain unchanged.

Run the profile conformance and status refusal regressions with
`go test -count=1 ./cmd/curator-engines ./internal/infra -run 'Test(StatusReadings|ReviewMalformedResourceEnvelope|Panel.*Alias|MainCatalogAdmits|Pinned|RetainedLocalEngineReadings)'`.
The retained-engine test reports unavailable observations as a skip, never a
healthy pass. Run the module-wide AST guard with
`go test -count=1 ./scripts -run '^TestReadingsRefusalCensusAndGeneratedSingletons$'`;
use the existing `scripts/engine-catalog-mutants.go` with
`-files internal/infra/engine_readings.go,internal/infra/engine_readings_envelopes_darwin.go -task TASK-260929-2j6yml` for scoped
mutation evidence. Logs and generated tables belong under `.temp/`.

Status capacity observations and `check` share the option table in
`internal/infra/engine_check.go`, including llama-cpp `-c` and `-ub` aliases.
MLX capacity entries in the same table remain outside the llama-cpp check subset.
Every evidence envelope on the readings path (hello, status, resource and all
nested objects) is decoded through one shared strict decoder with exact-case
closed member sets; unknown, duplicate, null and missing required members
refuse, and readings validate all enum, range, policy and classification
members before publishing any live values. Required sets mirror the owner
structs and are pinned by `TestSharedWireRequiredMatchesOwnerTags`; the
generated `TestWireRequiredMembersMatrix` removes and nulls every required
member of every envelope. Sharing ranges and content name the producer's own
bounds (`TestWireSharingAcceptsEveryCatalogFamily` and the TOML agreement
batteries prove the agreement). Run the argv checker regressions
with `go test -count=1 ./cmd/curator-engines -run '^TestEngineCheck'`.

Build identification: `.temp/curator-engines version [--json]` reports
`module_version`, `vcs_revision`, `vcs_time`, `vcs_modified`, and `go_version`
from Go build metadata. JSON values are strings; missing fields are `unknown`.
`.temp/` and `.codex/` are ignored scratch directories.

Weights verification caches live under `<cache-root>/weights-verification/`:
a `0700` directory with atomic `0600` JSON entries. The key includes resolved
path, device, inode, size, mtime ns, and ctime ns. Invalid or unsafe entries are
cache misses. A matching same-owner private entry is authoritative by design;
this cache protects against accidental changes and other users, not the same uid.
HFS+ and other 1-second-timestamp filesystems can miss same-size rewrites in one
second. Use `ensure --no-weights-cache` for a fresh byte hash on every ensure,
including attachment to a running broker. The flag bypasses cache reads/writes;
cold ensure may hash twice. Status reports the broker observation without hashing.
The catalog weights table accepts only `id`, so `cache = false` is refused.
`weights compute` and offline `check` still hash the full file;
their output contracts are unchanged. Verified live status also publishes the
`weight-artifact` reading for a clean absolute GGUF path; an unrepresentable
path degrades that fact without changing digest verification. It does not claim
weight residency or memory accounting.
See the protocol spec for the cache trust boundary and residual limits.
