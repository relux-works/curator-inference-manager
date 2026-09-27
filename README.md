# curator-inference-manager

The Curator inference plane. `curator-inference-manager` (renamed from curator-engine on 2026-09-24) is an umbrella provider that owns local inference engines for coding agents: llama.cpp, mlx-lm and mlx-swift. Its binary is `curator-engines`, so people and tools reach it as `curator engines …`: `list|show|add|remove|check|bench|confirm` for the catalog and `status|ensure|release|stop` for the engines (the command-line shape adopted on 2026-09-27). It holds a machine-local engine catalog, reports whether each engine is ready or can be ensured, grants leases, and runs engine lifecycle under host arbitration. It is carved out of the engine broker in the deprecated agents-infra residual.

**Status: DRAFT design. No code yet; nothing here is normative.** Its decision record will be Decision 0020 in curator-spec (not written yet).

## Layout

| Path | Content |
| --- | --- |
| `spec/inference-plane.md` | draft specification, extracted from the launch-profiles draft and the ecosystem roadmap |
| `spec/inference-plane.ru.md` | Russian translation of the draft specification (the English file is canonical) |

## How it fits

- Curator discovers `curator-engines` as an umbrella provider on a trust root, where it ships beside `curator` or is put by `curator provider install engines`; Curator itself stays engine-blind.
- Runtime bindings in the machine catalog `~/.curator/runtimes.toml` reference engine entries in `~/.curator/engines.toml` by name. Adding an engine is a widening entry that takes effect after `curator engines confirm`. The agents-management module models engine kinds and lifecycle plans as values and never starts processes.
- Callers: task-board preflight calls `ensure` for engine-bound runtimes (a dry run never ensures), and `curator-run` calls it before building the launch. The launch itself is built by the agents-management launch plane.
- Roadmap: milestone M5 in `wiki/roadmap/ecosystem-roadmap.md`.
