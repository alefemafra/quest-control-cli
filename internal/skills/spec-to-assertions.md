---
name: spec-to-assertions
description: >
  Batch-mode skill: receives a spec (and optional design/implementation docs)
  and emits black-box validation assertions as a single JSON array. No file
  reads, no tools, no interaction. First call of the spec-to-mission v2
  pipeline; output feeds spec-to-features as input.
---

# /spec-to-assertions

Single-pass derivation of validation-contract assertions from a spec. All
inputs are inlined in the prompt. Do NOT use any tools (Read, Bash, Grep, etc.).

## Context

You receive these blocks of input, all inline:

1. **Spec** — the full `spec.md` content
2. **Design Prompt** — optional, when the spec needs visuals
3. **Implementation Plan** — optional, when one was authored
4. **Coverage Requirements** — a programmatically-derived checklist of FRs,
   NFRs, and API endpoints your output MUST cover

Everything is inline. Do not attempt to read files.

## Workflow

### Step 1 — Internalize the spec

Hold these in working memory:

- **Goal** — what and why
- **Functional Requirements (FRs)** — numbered list, each independently testable
- **Non-Functional Requirements (NFRs)** — perf, a11y, error, security, telemetry
- **Data** — schemas, endpoints, types, entities (these become `data.*` and `api.*`)
- **State** — URL, form, client state
- **Auth & Tenancy** — permissions, scope (these become `auth.*`)
- **A11y / Telemetry** — explicit sections (these become `a11y.*` / `telemetry.*`)
- **Open Questions** — flag if any block testability

### Step 2 — Derive assertions

For EVERY FR and NFR, derive one or more black-box assertions.

**Format:** `<category>.<N>: <precondition/input> -> <action> -> <observable result>`

**Categories:** `api`, `ui`, `data`, `auth`, `error`, `perf`, `a11y`, `telemetry`, `test`
(add domain-specific if the spec demands). IDs scoped per category starting from 1.

**Rules:**

- Each assertion describes an **observable behavior from the outside** — input +
  action + expected output/state.
- NEVER reference implementation details (class names, function names, file paths).
- A validator who has never seen the code must verify each assertion.
- If a FR has 3+ distinct behaviors, produce 3+ assertions (happy + error + edge).
- NFRs become assertions: latency thresholds, a11y, error handling, security.
- If the spec lists API endpoints, produce >=1 happy AND >=1 error assertion per endpoint.
- If the spec has a schema table, derive `data.*` for field constraints.

**Examples:**

```
FR: "User enters name (required, max 255 chars)"
-> ui.1: Empty name field -> submit -> validation error shown
-> ui.2: Name exceeding 255 chars -> field enforces max length
-> data.1: Created record has name matching user input exactly

FR: "Slug auto-derived from name (kebab-case)"
-> ui.3: Type "My Event" in name -> slug shows "my-event" automatically
-> ui.4: User can manually override the auto-derived slug

NFR: "Step transitions < 200ms"
-> perf.1: Click Next on any step -> next step renders within 200ms

NFR: "All form fields have visible labels"
-> a11y.1: Every input has an associated visible <label> element

API: "POST /api/events"
-> api.1: POST /api/events with valid body -> 201 with created event
-> api.2: POST /api/events with missing required field -> 400 with validation error
-> api.3: POST /api/events without manage_events permission -> 403
```

### Quality bar

- "User can manage events" is NOT testable — decompose into concrete behaviors.
- Every assertion independently verifiable.
- Cover happy path, error cases, edge cases.
- The Coverage Requirements block enumerates FRs/NFRs/endpoints — every item
  there MUST appear in at least one assertion.

### Step 3 — Output

Output ONLY a valid JSON array of assertion groups. No markdown, no explanation,
no code fences.

```json
[
  {
    "category": "data",
    "items": [
      "data.1: Valid record input -> create -> record persisted with correct fields",
      "data.2: Missing required field -> create -> validation error returned"
    ]
  },
  {
    "category": "ui",
    "items": [
      "ui.1: Empty name field -> submit -> validation error shown"
    ]
  }
]
```

## Output rules

- One group per category. Categories must match: `api`, `ui`, `data`, `auth`,
  `error`, `perf`, `a11y`, `telemetry`, `test` (or domain-specific).
- IDs are scoped per category, starting from 1, sequential.
- Each assertion item begins with its ID followed by `: `.
- Output ONLY the JSON array — no prose, no markdown, no fences.

## Anti-patterns

- **Shallow spec reading.** Missing the schema table means assertions miss
  field constraints. Internalize EVERYTHING before generating.
- **One assertion per requirement.** Most FRs have multiple observable behaviors.
- **Implementation-leaked assertions.** "EventService.create() returns entity"
  is wrong. "POST /api/events with valid body returns 201" is right.
- **Ignoring NFRs.** A11y, perf, error handling, telemetry are real assertions —
  do not lump as "polish".
- **Missing the Coverage Requirements list.** Every item in that block MUST
  show up in at least one assertion.
