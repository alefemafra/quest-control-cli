---
name: spec-to-features
description: >
  Batch-mode skill: receives a spec, codebase analysis, and pre-derived
  assertion IDs, then emits ONLY a feature decomposition. No knowledge
  seeding (handled by spec-to-knowledge skill in the next phase). No file
  reads, no tools, no interaction.
---

# /spec-to-features

Single-pass conversion from spec + assertion IDs -> features. Knowledge is
NOT generated here — a dedicated downstream phase handles it. All inputs are
inlined. Do NOT use any tools.

## Context

You receive these blocks of input, all inline:

1. **Spec** — the full `spec.md` content
2. **Codebase Analysis** — pattern reference, domain model, file paths
3. **Design Prompt** — optional
4. **Implementation Plan** — optional
5. **Assertion IDs** — pre-derived list of assertion IDs grouped by category
   (the assertion text already exists in the validation contract — your job is
   to bucket the IDs into features, not re-author them)
6. **Decomposition Requirements** — programmatically-derived constraints your
   output MUST satisfy

Everything is inline. Do not attempt to read files.

## Workflow

### Step 1 — Map assertion IDs to feature buckets

Skim the Assertion IDs block. Group IDs by what would be implemented together
in a single worker session:

- `data.*` IDs typically belong to a foundation feature (schemas, types, fixtures)
- `api.*` IDs may belong with data (mock handlers) or with their consuming feature
- `ui.*` IDs split by component / page / form
- `a11y.*` and `perf.*` often map to a polish feature, OR fold into the feature
  that owns the relevant component if the assertion is component-specific
- `telemetry.*` often belongs to a single observability feature
- `auth.*` typically gates an entire route or page

### Step 2 — Decompose into features

Quality of decomposition determines quality of execution.

**Principles:**

- Each feature completable in **ONE worker session** (1-4 FRs, 2-5 assertions).
- Each feature **independently validatable** (unless declared in `depends_on`).
- Order by **dependency**: schemas -> hooks -> components -> integration -> polish.
- Feature with >5 `validation_refs` -> split it. Target 2-5 refs per feature.
- Feature with >6 `validation_refs` -> MUST split. Workers fail reliably above this threshold.
- Feature with 0 `validation_refs` -> unclear scope; every feature must be validatable.
- Prefer more focused features over fewer large ones. A feature with 11 refs (e.g., ui + data + a11y + perf + telemetry) should be 2-3 features.
- `depends_on` must be accurate — if F04 uses hooks from F03, declare it.

**Phase model:**

```
Phase 0 — Foundation: schemas, types, mock data (no dependencies)
Phase 1 — Core: hooks, main page, forms (depends on Phase 0)
Phase 2 — Integration: cross-cutting, sub-components (depends on Phase 1)
Phase 3 — Polish: tests, stories, a11y compliance (depends on Phase 2)
```

Adjust granularity to spec complexity: 5-FR spec -> 3-5 features, 20+ FR -> 10-15.

**Scope must be SPECIFIC:**

BAD: "Implement step 1"
GOOD: "RHF form with Zod resolver for EventBasicsSchema (name, slug, description).
Auto-derive slug from name. Validation on required fields. POST /api/events on submit."

Use the codebase analysis's **Reference Pattern** section to guide file paths,
naming conventions, and architectural alignment in each feature's scope.

### Step 3 — Output

Output ONLY a valid JSON object with the `features` key. NO knowledge field
(that is the next phase's job). No markdown, no explanation, no code fences.

```json
{
  "features": [
    {
      "id": "F01",
      "title": "<concise title>",
      "phase": 0,
      "depends_on": [],
      "scope": "<detailed — specific enough for a worker with NO prior context>",
      "description": "<why this feature exists, boundaries, and implementation cues>",
      "validation_refs": ["data.1", "data.2"]
    }
  ]
}
```

## Output rules

- Feature ID format: `F01`, `F02`, etc.
- Every assertion ID provided in the input MUST be referenced by >=1 feature.validation_refs.
- Every feature MUST have non-empty scope (target >=80 chars) describing schemas,
  validation rules, file paths, and API calls.
- Every feature MUST include `description` (target >=120 chars) with intent,
  boundaries, edge cases, and execution notes for the worker.
- Every feature MUST have >=1 validation_refs entry.
- Order features by `phase` (0..3) then by dependency.
- `depends_on` must be accurate — list the IDs of features that produce code
  this feature consumes.
- Output ONLY the JSON object with `features` — no `knowledge` field, no prose,
  no markdown, no fences.

## Anti-patterns

- **Vague feature scope.** "Build step 1" tells a worker nothing. List fields,
  schemas, validation rules, API calls.
- **Scope-only output.** If `description` is missing, the worker lacks context
  about intent, boundaries, and failure traps.
- **Dropping assertion IDs.** If `ui.7` is not referenced by any feature, that
  behavior won't be implemented — every ID must be assigned.
- **One giant feature.** If a feature has >5 `validation_refs`, split it. Workers fail reliably with 6+ refs.
- **Ignoring the codebase analysis.** The reference pattern exists so features
  follow existing conventions — use it for file paths and structure.
- **Including knowledge.** Knowledge is generated by a separate downstream
  phase that has access to your features.json. Do not include `knowledge` in
  your output.
