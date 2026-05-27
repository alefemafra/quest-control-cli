---
name: quest-spec
description: >
  Spec-driven entry point for Missions Architecture. Takes a raw feature idea,
  produces a self-contained feature folder at docs/specs/<slug>/ with spec.md,
  validation-contract.md, features.json, knowledge-base.md, and optionally a
  design prompt. Everything the quest engine needs lives inside that folder.
  Use when the user says "spec this", "plan a feature", "quest spec for X",
  "create a quest spec", or invokes /quest-spec (legacy: /mission-spec). Do NOT use for trivial
  fixes (no spec needed) or when a spec already exists for this feature.
---

# /quest-spec

One command to go from raw feature idea → complete spec → validation-contract
assertions → features.json decomposition → ready for quest-orchestrator.

Everything lives in `docs/specs/<slug>/`. If `docs/` doesn't exist, create it.

## What this skill produces

All output goes into one self-contained folder:

```
docs/specs/<slug>/
├── spec.md                      # full technical spec
├── design-prompt.md             # optional, if design is needed
├── designs/                     # screenshots from design tool
└── quest/   # legacy name supported: mission/
    ├── validation-contract.md   # black-box assertions for this feature
    ├── features.json            # task decomposition for this feature
    ├── knowledge-base.md        # decisions and learnings
    └── runs/                    # critic + worker + validator reports
```

## When NOT to use

- Trivial fix (typo, config, 1-2 lines) — no spec needed, go straight to worker.
- Feature already has a `docs/specs/<slug>/` folder with spec and assertions — use
  `mission-orchestrator` directly.
- Project doesn't follow Missions Architecture yet — run `init-mission` first.
- Exploration phase — talk to the user directly, don't spec prematurely.

## Pre-flight checks (run BEFORE any questions)

1. Read `CLAUDE.md` — understand project stack, architecture, inviolable rules.
   If CLAUDE.md doesn't exist, warn the user to run `init-mission` or `/init`
   first.
2. If `docs/` directory does not exist → create it.
3. Scan `docs/` for existing feature folders — if `docs/specs/<slug>/spec.md`
   already exists for this feature, surface it and ask: revision or new work?
4. If revising, read the existing `docs/specs/<slug>/mission/` artifacts to
   understand current state.

## Workflow

### 1. Capture author identity

Run `git config user.name` and `git config user.email`.
Confirm via `AskUserQuestion` with the detected identity as the recommended
option. This becomes `owner` and `created_by` in frontmatter.

### 2. Round 1 — orientation questions

Ask as a **single `AskUserQuestion` call** (up to 4 questions):

1. **What are we building?** (free-form) — the feature idea, problem, or
   ticket reference. If the user already described it in conversation, skip
   this and use what they said.
2. **Surface type**: New feature / Extends existing feature / Refactor /
   Infrastructure / Integration
3. **Design needed**: Yes — I need mockups or design guidance / No —
   implementation is clear
4. **Estimated complexity**: Small (1-2 features) / Medium (3-5 features) /
   Large (6+ features)

### 3. Round 2 — scope questions (one at a time, NOT bundled)

1. "What files or directories will this touch? List as paths or globs."
   (Wait for answer, acknowledge briefly, continue.)

2. "Any new domain terms introduced that aren't in the project docs?"
   (If yes, note them for knowledge-base.)

3. "Any architectural decision needed — new dependency, API contract,
   infrastructure change?" (If yes, note for knowledge-base entry.)

4. "Any external systems involved — APIs, databases, queues, third-party
   services?" (If yes, capture for Data section.)

### 4. Write the spec

Create the full folder structure:

```bash
mkdir -p docs/specs/<slug>/designs
mkdir -p docs/specs/<slug>/mission/runs
```

Copy the template from `~/.claude/skills/mission-spec/templates/spec.md`.
Fill in every `{{...}}` placeholder using the interview answers and project
context from CLAUDE.md.

**Rules for writing:**

- `## Goal` must be 2-4 sentences, specific and concrete.
- `## Functional Requirements` must be numbered, each independently testable.
  These are the source material for validation-contract assertions — write them
  as observable behaviors, not implementation details.
- `## Non-goals` must be filled (prevents scope creep later).
- `## Scope` must have real paths, not vague pointers.
- Leave `<!-- TODO -->` markers ONLY where the user explicitly said they
  don't know yet.

Write to `docs/specs/<slug>/spec.md`. Set frontmatter `status: draft`.

### 5. Design prompt (conditional)

If the user said **Yes** to design needed in Round 1:

Copy `~/.claude/skills/mission-spec/templates/design-prompt.md`. Fill in
placeholders using spec context + project's design language from CLAUDE.md.

Write to `docs/specs/<slug>/design-prompt.md`.

Tell the user:

> "Feed `docs/specs/<slug>/design-prompt.md` to your design tool.
> Save output to `docs/specs/<slug>/designs/` and update `## Design
> References` in the spec."

### 6. Convert spec → validation-contract assertions

This is the core bridge step. For each Functional Requirement and
Non-Functional Requirement in the spec, derive one or more black-box
assertions.

**Assertion derivation rules:**

- Each assertion must describe an **observable behavior** from the outside —
  input + action + expected output/state. Never reference internal
  implementation (class names, function names, variable names).
- Use the format: `<category>.<N>: <input/precondition> → <action> →
  <observable result>`
- Group assertions into categories. Default categories:
  `api.*`, `ui.*`, `data.*`, `auth.*`, `error.*`, `perf.*`
- Assertion IDs are scoped to this feature — start from `1` per category.
  No need to worry about global collisions since each feature has its own
  contract.
- Non-functional requirements become assertions too: performance thresholds,
  error handling behaviors, accessibility requirements, security constraints.

**Example conversions:**

```
Functional Req: "User can create a new event with name, date, and venue."
→ api.1: POST /events with valid {name, date, venue_id} returns 201 with created event
→ ui.1: Event creation form shows validation errors for missing required fields
→ ui.2: After successful creation, user is redirected to event detail page

Non-Functional Req: "Event creation must respond within 500ms p95."
→ perf.1: POST /events p95 latency < 500ms under normal load

Non-Functional Req: "Only users with manage_events permission can create events."
→ auth.1: POST /events without manage_events permission returns 403
```

**Present the assertions to the user before writing.** Show them grouped
by category with the assertion IDs. Ask via `AskUserQuestion`:

- "These assertions look correct — write the contract"
- "I have changes" → apply changes, re-present
- "Stop — I need to rethink the requirements" → stop

Once confirmed, write to `docs/specs/<slug>/mission/validation-contract.md`:

```markdown
# Validation Contract — <Feature Name>

Behavioral assertions this feature must satisfy. Validators exercise the
system black-box and report PASS/FAIL/BLOCKED per assertion.

Each assertion has a **stable ID** (e.g., `category.N`) — features in
`features.json` reference these IDs via `validation_refs`.

---

## <Category>

- **<id>** <assertion text>
...

---

## How the validator uses this contract

1. Read this file FIRST, before anything else.
2. Identify assertions for the feature under validation (`validation_refs`
   in `features.json`).
3. Exercise the system black-box (as a real user).
4. For each assertion: PASS / FAIL / BLOCKED + concrete evidence.
5. Report list. Never propose fixes.
```

### 7. Decompose spec → features.json entries

Break the spec into implementation features. Each feature should be:

- **Small enough** for one worker session (1-3 functional requirements max).
- **Independently validatable** — its `validation_refs` assertions can be
  tested without other features being done (unless declared in `depends_on`).
- **Ordered by dependency** — data layer before UI, schema before API hooks,
  core before consumers.

**Decomposition pattern:**

```
Feature 1: Data layer (schemas, API hooks, types)
  → validation_refs: [data.*, api.*]
  → depends_on: []

Feature 2: Core UI (main component, layout, navigation)
  → validation_refs: [ui.1, ui.2, ...]
  → depends_on: [Feature 1]

Feature 3: Forms & validation (input, validation, submission)
  → validation_refs: [ui.3, ui.4, error.*]
  → depends_on: [Feature 1]

Feature 4: Auth & permissions (guards, tenant scoping)
  → validation_refs: [auth.*]
  → depends_on: [Feature 1]

Feature 5: Polish (performance, a11y, observability)
  → validation_refs: [perf.*, a11y.*]
  → depends_on: [Feature 2, Feature 3]
```

Adjust the number and granularity based on the user's complexity estimate
from Round 1. Small = 1-2 features, Medium = 3-5, Large = 6+.

**Feature ID format:** `F01`, `F02`, etc. Sub-features: `F01a`, `F01b`.

**Present the decomposition to the user before writing.** Show each feature
with its scope, validation_refs, and depends_on. Ask via `AskUserQuestion`:

- "Decomposition looks correct — write features.json"
- "I have changes" → apply changes, re-present
- "Stop — scope needs rethinking" → stop

Once confirmed, write to `docs/specs/<slug>/mission/features.json`:

```json
{
  "spec": "docs/specs/<slug>/spec.md",
  "owner": "<Name <email>>",
  "status_lifecycle": ["pending", "in_progress", "awaiting_validation", "validated", "done", "blocked"],
  "features": [
    {
      "id": "F01",
      "title": "<short description>",
      "status": "pending",
      "depends_on": [],
      "scope": "<what needs to be done>",
      "validation_refs": ["api.1", "ui.1"]
    }
  ],
  "fix_features": []
}
```

### 8. Seed knowledge-base

Write `docs/specs/<slug>/mission/knowledge-base.md`:

```markdown
# Knowledge Base — <Feature Name>

Workers and validators accumulate findings here.

## How to contribute

Each entry starts with `## YYYY-MM-DD — short title`. Workers and validators
APPEND new entries; they DO NOT edit others' entries.

---

## YYYY-MM-DD — Spec created

Spec: `docs/specs/<slug>/spec.md`.
<1-2 sentences on key decisions or constraints>.
Assertions: <count> across <categories>.
Features: <list of feature IDs>.
```

### 9. Run mission-critic (if available)

If the mission-critic skill is available:

1. Run **Phase A** (validation-contract review) — the new assertions should
   pass the critic's quality bar.
2. Run **Phase C** (decomposition review) — the new features should be
   well-decomposed.

Reports go to `docs/specs/<slug>/mission/runs/`.

If critic finds issues, present them to the user. Do NOT auto-fix — the user
decides whether to revise or override.

If mission-critic is not available, skip and note it in the handoff.

### 10. Validate before presenting final summary

- [ ] All `{{...}}` placeholders replaced in spec?
- [ ] `## Goal` is 2-4 sentences and specific?
- [ ] `## Scope` has real paths?
- [ ] `## Non-goals` filled in?
- [ ] Every Functional Requirement has at least one assertion?
- [ ] Every assertion has a stable ID?
- [ ] Every feature has `validation_refs` and `depends_on`?
- [ ] `design-prompt.md` written if design was requested?
- [ ] `knowledge-base.md` seeded?

### 11. Hand off

Tell the user:

- Feature folder: `docs/specs/<slug>/`
- Spec: `docs/specs/<slug>/spec.md`
- Contract: `docs/specs/<slug>/mission/validation-contract.md`
- Features: `docs/specs/<slug>/mission/features.json`
- Design prompt (if generated): `docs/specs/<slug>/design-prompt.md`
- Critic result (if run)
- **Next steps** (in order):
  1. If design needed: feed the design prompt to your tool, drop screenshots,
     update `## Design References`.
  2. Review the spec one more time. Update `status: active` when satisfied.
  3. Run `mission-orchestrator docs/specs/<slug>` to start the execution cycle.

## Anti-patterns

- **Vague Functional Requirements.** "User can manage events" is not testable.
  "User can create an event with name, date, and venue" is. If you can't
  derive a concrete assertion, the requirement is too vague — push back.
- **Assertions that reference implementation.** "`EventService.create()` returns
  the entity" is an implementation leak. "POST /events returns 201 with the
  created event" is black-box. The validator must never need to read source code
  to verify an assertion.
- **One giant feature.** If a feature has more than 5 `validation_refs`, it's
  too big. Split it.
- **Features without `validation_refs`.** Every feature must be validatable.
  If you can't point to assertions, the feature's scope is unclear.
- **Skipping user confirmation on assertions and decomposition.** These are
  the contract the team builds against — never auto-commit them.
- **Speccing before understanding.** If the user can't answer Round 2
  questions, they need exploration, not a spec.
- **Over-decomposing small work.** A 2-assertion feature doesn't need 5
  sub-features. Match granularity to complexity.
