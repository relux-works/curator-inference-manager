# curator-inference-manager

The Curator inference plane. `curator-inference-manager` (renamed from curator-engine on 2026-09-24) is an umbrella provider that owns local inference engines for coding agents: llama.cpp, mlx-lm and mlx-swift. Its binary is `curator-engines`, so people and tools reach it as `curator engines …`: `list|show|add|remove|check|bench|confirm` for the catalog and `status|ensure|release|stop` for the engines (the command-line shape adopted on 2026-09-27). It holds a machine-local engine catalog, reports whether each engine is ready or can be ensured, grants leases, and runs engine lifecycle under host arbitration. It is carved out of the engine broker in the deprecated agents-infra residual.

The inference-plane design remains a draft; the extracted broker wire contract is normative in [`spec/engines-protocol-v1.md`](spec/engines-protocol-v1.md). That v1 extraction preserves the versioned `pi_shared` wire fields, exit codes, and refusal codes while adding engine identity and process-lifetime lease fields. Decision 0020 is tracked by curator-spec issue #104.

## Layout

| Path | Content |
| --- | --- |
| `spec/inference-plane.md` | draft specification, extracted from the launch-profiles draft and the ecosystem roadmap |
| `spec/inference-plane.ru.md` | Russian translation of the draft specification (the English file is canonical) |

## Tools

| Tool | Use | Run | Outputs |
| --- | --- | --- | --- |
| Go toolchain | Build and validate the broker and CLI | `go build ./...`; `go vet ./...`; `go test ./...` | Build output stays in Go's cache; package results go to stdout/stderr. |
| `golangci-lint` | Lint Go source on the configured host platform | `golangci-lint run` | Findings go to stdout/stderr; no repository artifacts are generated. |
| ShellCheck | Lint the non-Darwin validation script | `shellcheck scripts/test-nondarwin-emu.sh` | Findings go to stdout/stderr; no repository artifacts are generated. |
| `curator-engines` | Inspect or manage configured local engine runtimes | `go build -o .temp/curator-engines ./cmd/curator-engines`, then `.temp/curator-engines status --json` (or `ensure`, `release`, `stop`) | Binary at `.temp/curator-engines`; status is printed to stdout; broker state follows `spec/engines-protocol-v1.md`. |
| Non-Darwin file-set emulator | Run Linux and Windows file-set validation on the current host | `./scripts/test-nondarwin-emu.sh` | Temporary trees under `${TMPDIR:-/tmp}` are removed at exit; commands and results are printed to stdout/stderr. For an isolated mutant, `./scripts/test-nondarwin-emu.sh --materialize linux <source-root> <output-dir>` materializes one verified file set and retains it at `<output-dir>`. |
| Python 3 standard library | Parse `go list -json` manifests and compare the target and emulated `GoFiles`/`TestGoFiles` sets | Invoked by `./scripts/test-nondarwin-emu.sh`; no separate project command | Comparison metadata and helper are created under the script's temporary directory and removed at exit. |

## How it fits

- Curator discovers `curator-engines` as an umbrella provider on a trust root, where it ships beside `curator` or is put by `curator provider install engines`; Curator itself stays engine-blind.
- Runtime bindings in the machine catalog `~/.curator/runtimes.toml` reference engine entries in `~/.curator/engines.toml` by name. Adding an engine is a widening entry that takes effect after `curator engines confirm`. The agents-management module models engine kinds and lifecycle plans as values and never starts processes.
- Callers: task-board preflight calls `ensure` for engine-bound runtimes (a dry run never ensures), and `curator-run` calls it before building the launch. The launch itself is built by the agents-management launch plane.
- Roadmap: milestone M5 in `wiki/roadmap/ecosystem-roadmap.md`.
