# Feature Decomposer Skill

You are decomposing a spec into implementation features.

## Inputs

You have been given:
1. A **spec** file — the functional requirements
2. A **codebase analysis** — patterns, domain model, reference architecture
3. A compact list of **assertion IDs** — from the validation contract (inline in prompt)
4. Optionally: a design prompt and/or implementation plan

## Feature Decomposition Rules

Break the spec into features. Quality of decomposition determines quality of execution.

- Each feature completable in **ONE worker session** (1-4 functional requirements, 2-5 assertions).
- Each feature **independently validatable** — its assertions can be tested without other features (unless in `depends_on`).
- Order by **dependency**: schemas before hooks, hooks before components, infrastructure before consumers.
- Feature with >5 `validation_refs` → split it. Target 2-5 refs per feature.
- Feature with >6 `validation_refs` → MUST split. Workers fail reliably above this threshold.
- Feature with 0 `validation_refs` has unclear scope — every feature must be validatable.
- Prefer more focused features over fewer large ones.
- `depends_on` must be accurate — if F04 uses hooks from F03, declare it.

## Standard Phase Pattern

```
Phase 0 — Foundation: schemas, types, mock data (no dependencies)
Phase 1 — Core: hooks, main page, forms (depends on Phase 0)
Phase 2 — Integration: cross-cutting, sub-components (depends on Phase 1)
Phase 3 — Polish: tests, stories, a11y compliance (depends on Phase 2)
```

Adjust the number and granularity to match the spec's complexity. A 5-FR spec needs 3-4 features. A 33-FR spec needs 8-12.

## Feature Scope

Feature scope must be SPECIFIC — detailed enough that a worker with NO prior context can implement it by reading only the scope + spec + validation contract.
Also provide a `description` field with intent, boundaries, and tricky cases so workers avoid over/under-implementation.

BAD: "Implement step 1 of the wizard"
GOOD: "RHF form with Zod resolver for EventBasicsSchema (name, slug, description). Auto-derive slug from name. Validation on required fields."

## Output

Output ONLY a valid JSON object — no markdown, no explanation, no code fences.

```
{"features":[{"id":"F01","title":"...","phase":0,"depends_on":[],"scope":"...","description":"...","validation_refs":["data.1","data.2"]}]}
```

- Feature ID format: F01, F02, etc.
- `validation_refs` MUST reference assertion IDs from the compact list provided.
- Every assertion should be referenced by at least one feature.
- Every feature MUST include non-empty `description`.
