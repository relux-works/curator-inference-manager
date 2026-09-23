# curator-engine

The Curator inference plane. `curator-engine` is an umbrella provider (`curator engine status|ensure|release|stop`) that owns local inference engines for coding agents: llama.cpp, mlx-lm and mlx-swift. It holds a machine-local engine catalog, reports whether each engine is ready or can be ensured, grants leases, and runs engine lifecycle under host arbitration. It is carved out of the engine broker in the deprecated agents-infra residual.

**Status: DRAFT design. No code yet; nothing here is normative.** Its decision record will be Decision 0020 in curator-spec (not written yet).

## Layout

| Path | Content |
| --- | --- |
| `spec/inference-plane.md` | draft specification, extracted from the launch-profiles draft and the ecosystem roadmap |

## How it fits

- Curator discovers it as an umbrella provider on a trust root; Curator itself stays engine-blind.
- Runtime bindings in the agents-management machine catalog (`runtimes.toml`) reference engine profiles in `engines.toml` by name. The agents-management module models engine kinds and lifecycle plans as values and never starts processes.
- Callers: task-board preflight calls `ensure` for engine-bound runtimes (a dry run never ensures), and `curator-run` calls it before building the launch. The launch itself is built by the agents-management launch plane.
- Roadmap: milestone M5 in `wiki/roadmap/ecosystem-roadmap.md`.
