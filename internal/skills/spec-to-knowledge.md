---
name: spec-to-knowledge
description: >
  Batch-mode skill: receives a spec, codebase analysis, and the already-decomposed
  features.json, then emits actionable knowledge entries that workers and
  validators consume. Final synthesis phase before the critic gate. No file
  reads, no tools, no interaction.
---

# /spec-to-knowledge

Single-pass extraction of actionable knowledge from spec + analysis +
decomposed features. Knowledge anchors the worker pipeline — every entry must
save a worker time or prevent a mistake. All inputs are inlined.

## Context

You receive these blocks of input, all inline:

1. **Spec** — the full `spec.md` content
2. **Codebase Analysis** — pattern reference, domain model, file paths
3. **Features (already decomposed)** — the final `features.json` you must
   produce knowledge entries for
4. **Validation Contract (already authored)** — assertion summaries (optional)
5. **Design Prompt** / **Implementation Plan** — optional

You ARE NOT decomposing features. They are already authored. Your job is to
synthesize the knowledge entries that future workers and validators will rely
on while implementing those features.

## Workflow

### Step 1 — Identify knowledge buckets

Skim spec + analysis + features and map out:

- **Architectural decisions** explicit in the spec ("All API calls go through
  feature flag X", "Slug auto-derives from name")
- **Stack/pattern constraints** the codebase analysis surfaces ("All schemas
  use z.object().strict()", "Routes registered in src/routes.tsx via lazy()")
- **Reference pattern conventions** workers must follow ("Follow EventList
  pattern at src/features/events/ for route/module structure")
- **Provider/hook APIs** that differ from spec assumptions ("useTenant()
  returns Tenant or null, not undefined; check upstream docs")
- **Available libraries** to reuse ("@sitickets/datetime for UTC conversion",
  "shared Pagination component at src/components/Pagination.tsx")
- **Open questions** the spec flags as unresolved (workers must surface these
  rather than guessing)
- **External references** mentioned in spec or analysis (style guides, API
  docs, internal RFCs)

### Step 2 — Anchor each entry to a feature when possible

Knowledge entries are most valuable when they map to specific features. As
you write each entry, ask:

- "Which feature(s) consumes this knowledge?"
- "Will the worker implementing F03 read this and avoid a 30-min wrong path?"

If an entry doesn't help any current feature, drop it.

### Step 3 — Write actionable bullets

Each entry: a single, concrete, actionable insight.

**Good entries:**

- "All API mocks gated by VITE_USE_API_MOCKS env var; fall back to live
  endpoints when false (handlers in src/mocks/handlers/)"
- "Follow EventForm pattern at src/features/events/EventForm.tsx for RHF +
  Zod resolver setup; reuses shared Field component"
- "Schemas under src/features/<domain>/schema.ts use z.object().strict() to
  reject unknown fields — required for all new entities"
- "useTenant() returns null when no tenant is selected — guard before
  reading .id; do not throw"

**Bad entries (drop):**

- "The project uses TypeScript" (useless — workers know)
- "Spec calls for filters" (paraphrasing the spec adds nothing)
- "Be careful with state management" (vague — say WHAT to be careful about)

### Step 4 — Output

Output ONLY a valid JSON array of strings. No markdown, no explanation, no
code fences. No object wrapper.

```json
[
  "All API calls use MSW mocks, gated by VITE_USE_API_MOCKS",
  "Follow EventList pattern at src/features/events/ for route/module structure",
  "Schemas under src/features/<domain>/schema.ts use z.object().strict()"
]
```

## Output rules

- JSON array of strings, top-level — no wrapper object.
- Target 8–18 entries. More than 25 is dilution; less than 5 likely missing
  important conventions.
- Each entry: complete sentence (or punctuation-terminated phrase), 60–200
  characters.
- Every entry must be **actionable** (a worker takes a different action after
  reading it).
- Do NOT include the spec text itself, FR numbers, or feature IDs verbatim —
  knowledge is the synthesis around them.
- Output ONLY the JSON array — no prose, no fences, no `{}` wrapper.

## Anti-patterns

- **Knowledge as paraphrase.** Restating the spec is not knowledge — workers
  already have the spec.
- **Knowledge as dump.** Listing every fact in the analysis dilutes the
  signal. Each entry must save time or prevent a mistake.
- **Vague guidance.** "Be careful with auth" is useless. "useAuth() returns
  null until first /me response; gate routes with <RequireAuth>" is useful.
- **Generic best practices.** "Write tests" / "handle errors" — workers
  know. Project-specific or feature-specific only.
- **Wrapper object.** Output the array directly. No `{"knowledge": [...]}`.
