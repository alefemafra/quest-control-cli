# Quest Dashboard

Quest Dashboard is a Go TUI that orchestrates spec-driven delivery with Claude subprocesses.
It runs a full execution pipeline over a target project, coordinating critic gates, workers,
validators, and refinement loops from one interface.

`quest/` is now the primary artifact directory name. Legacy `mission/` folders are still supported.

## What This Project Does

- Runs interactive spec discovery and plan generation.
- Gates plans with mechanical + judgment critic checks.
- Executes features in phased concurrency (parallel in phase, sequential across phases).
- Validates work black-box against a contract of assertions.
- Generates fix features from failed validations and re-enters the pipeline.

## Quick Start

```bash
go build -o quest .
go run .
go run . <slug>
go run . new
go test ./...
go vet ./...
```

Notes:
- `<slug>` resolves to `docs/specs/<slug>/` in the target project.
- You can still build legacy naming if needed: `go build -o mission .`.

## Pipeline Overview

1. Spec discovery
2. Critic gate
3. Workers
4. Validators
5. Refinement (when needed)

Feature lifecycle:

`pending -> in_progress -> awaiting_validation -> validating -> refining -> done|blocked`

## Required Spec Folder Shape

Each feature spec is expected under:

```text
docs/specs/<slug>/
├── spec.md
├── quest/
│   ├── features.json
│   ├── validation-contract.md
│   ├── knowledge-base.md
│   ├── project-context.md
│   ├── runs/
│   └── logs/
└── designs/               # optional
```

Legacy shape (still supported):

```text
docs/specs/<slug>/mission/
```

## Artifact Contracts

- `spec.md`: source-of-truth requirements for the feature.
- `quest/validation-contract.md`: black-box assertions with stable IDs.
- `quest/features.json`: decomposition, dependencies, statuses, and fix lineage.
- `quest/knowledge-base.md`: append-only implementation/validation learnings.
- `quest/runs/`: critic/worker/validator reports and runtime state.

## Supported Spec Folder Variants

- Flat slug (recommended): `docs/specs/my-feature/`
- Nested slug (supported with caveats): `docs/specs/domain/my-feature/`
- Spec-only discovery: folder with `spec.md` but no `features.json` is discoverable.
- Artifact-only discovery: folder with `quest/features.json` (or legacy `mission/features.json`) is discoverable.
- Optional files:
  - `design-prompt.md`
  - `implementation-plan.md`
  - `quest/codebase-analysis.md`
  - `quest/critique-criteria.local.md`

## Compatibility Rules

- New writes default to `quest/`.
- Existing specs continue to work under `mission/`.
- Mechanical checks auto-detect `quest/` first, then `mission/`.
- Skill loading supports both `quest-*` and legacy `mission-*` names.

## Development Notes

- Main orchestrator code lives in `internal/`.
- Embedded skills live in `internal/skills/`.
- Mechanical critic checks are in `internal/skills/checks/run-mechanical.mjs`.
