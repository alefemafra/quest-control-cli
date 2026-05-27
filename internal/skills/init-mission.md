---
name: init-quest
description: Bootstrap or adopt Missions Architecture in a project. Auto-detects greenfield vs brownfield - creates full structure for new projects (asks architecture, languages, frameworks, hosting), or appends to existing CLAUDE.md/README/.gitignore and creates quest/ folder for existing projects (legacy mission/ supported). Triggers - "init quest" / "/init-quest" / "bootstrap quest project" / "adopt missions architecture" / "retrofit quest" / "add quest to this project" (legacy: init mission / /init-mission).
---

# Init Quest

Bootstrap or adopt Missions Architecture. Works in EMPTY directories (greenfield) AND in directories with existing code (brownfield).

## When to use

- User just created a new directory and wants to start with Missions methodology (greenfield).
- User wants to adopt Missions methodology in an existing project (brownfield).
- User asks "how do I start a new project with this methodology?".
- User asks "how do I add Missions to this existing project?".

## When NOT to use

- Project already has `docs/specs/*/mission/features.json` (re-init is destructive — ABORT and warn user).
- User just wants to UNDERSTAND the methodology (point them to `~/.claude/projects/-Users-galileu-dev/memory/reference_missions_architecture.md`).

## Mode detection (run FIRST, before any questions)

Classify the current directory:

```
brownfield IF any of:
  - .git/ exists AND has at least one commit
  - package.json, pyproject.toml, Cargo.toml, go.mod, Gemfile, build.gradle, pom.xml, composer.json exists
  - Dockerfile, docker-compose.yml, vercel.json, fly.toml, railway.toml, render.yaml, netlify.toml, .github/workflows/ exists
  - more than 5 source files (*.ts, *.tsx, *.js, *.py, *.rs, *.go, *.rb, *.java, *.kt, *.php) outside node_modules / vendor / target

greenfield otherwise (empty dir, or only README/LICENSE/.gitignore stub)
```

State the detected mode to the user and let them override before proceeding.

---

## Procedure — GREENFIELD

### 1. Collect context (all 6 questions are required)

1. **Project name** (slug, e.g., `my-app`).
2. **One-sentence description** (goes in CLAUDE.md header).
3. **Architecture style** (e.g., "modular monolith", "serverless functions + DB", "static site", "CLI tool", "microservices", "library/SDK").
4. **Languages + frameworks** (e.g., "TypeScript + Next.js 15 + Prisma + Postgres + grammY").
5. **Hosting target** ("Railway", "Vercel", "Fly.io", "self-hosted Docker", "AWS Lambda", "undecided").
6. **Regulated domain?** (health, finance, legal, child education, none — affects inviolable rules in CLAUDE.md).

Do not create files until all 6 are answered. If user skips one, mark with `[TODO]` and proceed.

### 2. Initialize git

```bash
git init
```

### 3. Create full structure (see "File contents" below) — all files written fresh

### 4. Show next-steps summary

---

## Procedure — BROWNFIELD

### 1. Auto-detect (run in parallel, BEFORE asking anything)

- **Stack**: parse `package.json` / `pyproject.toml` / `Cargo.toml` / `go.mod` / `Gemfile` / `composer.json`.
- **Owner**: `git config user.name`.
- **Hosting**: presence of `Dockerfile`, `vercel.json`, `fly.toml`, `railway.toml`, `render.yaml`, `netlify.toml`, `.github/workflows/deploy*.yml`.
- **Project name**: from `package.json` `name`, or `pyproject.toml` `[project] name`, or directory basename as fallback.
- **Description**: from `package.json` `description`, or first non-title paragraph of existing `README.md`.
- **Architecture hint**: directory layout signals (`apps/` + `packages/` → monorepo, `services/` → microservices, single `src/` → monolith, `cmd/` + `pkg/` → Go binary, etc.).
- **External dependencies**: scan dependency manifests and map package names to external systems. Group by category and produce a bullet list. Mapping patterns (non-exhaustive):
  - **Databases**: `pg`, `mysql2`, `mongoose`, `mongodb`, `prisma` / `@prisma/client`, `sqlite3`, `better-sqlite3`, `drizzle-orm`, `typeorm`, `sequelize` → SQL/NoSQL store. In Python: `psycopg2`, `psycopg`, `asyncpg`, `pymongo`, `sqlalchemy`. In Rust: `sqlx`, `diesel`, `mongodb`. In Go: `lib/pq`, `pgx`, `gorm.io/driver/*`.
  - **Cache / sessions**: `redis`, `ioredis`, `memcached` → Redis/Memcached. Python: `redis-py`. Rust: `redis`. Go: `go-redis`.
  - **Queues**: `bullmq`, `amqplib`, `kafkajs`, `sqs-consumer`. Python: `celery`, `pika`, `aiokafka`. Rust: `lapin`, `rdkafka`.
  - **LLM providers**: `openai`, `@anthropic-ai/sdk`, `@google/generative-ai`, `cohere-ai`, `mistralai`. Python: `openai`, `anthropic`, `google-generativeai`.
  - **Payments**: `stripe`, `@stripe/*`, `braintree`. Python: `stripe`.
  - **Email**: `nodemailer`, `@sendgrid/mail`, `resend`, `postmark`. Python: `sendgrid`.
  - **SMS / messaging**: `twilio`, `@slack/web-api`, `grammy` (Telegram), `discord.js`.
  - **Object storage / cloud**: `@aws-sdk/*`, `aws-sdk`, `@azure/*`, `@google-cloud/*`, `minio`. Python: `boto3`.
  - **Auth providers**: `next-auth`, `auth0`, `passport-*`, `@clerk/*`, `@supabase/supabase-js` (also DB).
  - **Search / vector**: `elasticsearch`, `@elastic/elasticsearch`, `meilisearch`, `pinecone-client`, `@pinecone-database/pinecone`, `weaviate-client`, `qdrant-js`.
  - **Observability**: `@sentry/*`, `pino-pretty` → Sentry, structured logging. Treat as external if it ships logs/metrics off-process.

  Emit one bullet per detected category with version when available, marked `(auto-detected — review)`. Skip pure-utility libs (lodash, date-fns, axios, express, etc.) — they are not external systems.

### 2. Show detection, confirm, ask only what's missing

```
Detected:
  Project name: {detected}
  Stack:        {detected}
  Owner:        {detected}
  Hosting:      {detected or "(none — no deploy config found)"}
  Description:  {detected or "(missing)"}
  Architecture: {hint}

Confirm? [Y/n/edit]
```

**Do not ask additional questions in brownfield mode.** The team is adopting Missions onto existing code; they have one ticket they want to work on, not a product-planning session. Specifically:

- **Do NOT ask "regulated domain?"** — write a `[TODO: classify regulated domain if applicable]` placeholder in `CLAUDE.md`'s Inviolable Rules section. The dev (or tech lead) can fill it in when relevant. Forcing the question makes the tool feel like a survey instead of a bootstrap.
- **Do NOT ask "first 3–5 backlog features?"** — features are created per-feature via `mission-spec`, not upfront.
- Only ask for fields that came up empty from auto-detection AND that are required for the structure to be valid (e.g., project name if `package.json` has no `name`).

### 3. No global mission artifacts

Unlike the previous architecture, `validation-contract.md`, `features.json`,
and `knowledge-base.md` are NOT created at init time. They are created
**per feature** inside `docs/specs/<slug>/mission/` when the user runs `mission-spec`.
This avoids empty boilerplate and keeps each feature self-contained.

Write entries as real dated entries (`## YYYY-MM-DD — short title`), 3–10 lines each. Do NOT mark with `(auto-extracted — review)` — that's a smell that the entries aren't actionable. If you can't write the entry as a real, useful gotcha, leave it out.

### 5. Create / append files (NON-DESTRUCTIVE)

| File | Action |
|------|--------|
| `docs/` directory | CREATE if missing (feature folders go here via `mission-spec`) |
| `CLAUDE.md` | If exists: APPEND `## Methodology: Missions Architecture` section + show diff for user approval before writing. If missing: CREATE. |
| `README.md` | If exists: APPEND `## Methodology` section at the end. If missing: CREATE. |
| `.gitignore` | APPEND only lines that are missing (e.g., `.audit/`, `.dashboard/`). Never overwrite. |

NEVER overwrite `CLAUDE.md`, `README.md`, or `.gitignore`. Always read first, diff, and append at the bottom.

#### When an existing CLAUDE.md is missing required sections

The `mission-critic` mechanical checks require three sections in CLAUDE.md: `## Architecture`, `## External Dependencies`, and `## Inviolable rules` (or equivalent). After confirming the Methodology section is appended, scan the existing CLAUDE.md for these headings:

- If `## Architecture` (or `## Architecture / Stack`) is missing → append a stub with subsections **Style**, **Components** (auto-detected from the architecture hint), **Data flow** (`[TODO]`), **Failure modes** (`[TODO]`).
- If `## External Dependencies` is missing → append the bullet list produced by the auto-detection step. Do NOT mark entries with `(auto-detected — review)` — if you detected them, write them directly; if you couldn't detect anything, append `- [TODO: list external systems this project talks to]`.
- If `## Inviolable rules` is missing → append a `[TODO: classify regulated domain and list domain-specific rules if applicable]` placeholder + any user-global inviolable rules carried from memory (e.g., "no sourcemaps in production"). Do not block on the regulated-domain question.

Always diff before appending. The goal is that running `node ~/.claude/skills/mission-critic/checks/run-mechanical.mjs` on the project immediately after init passes the architecture checks (`M-A1` through `M-A4`).

### 6. Do NOT run `git init`

The project already has git history.

### 7. Show next-steps summary

---

## File contents

### `CLAUDE.md` (greenfield: write whole file. brownfield: append only the "Methodology" section onward.)

```markdown
# {Project name}

{One-sentence description}

## Architecture

**Style**: {architecture style}

**Components**:

- [TODO: list each component with a one-sentence responsibility AND what it does NOT do. Example: `AuthService — issues and validates session tokens. Does NOT store user profiles (UserService owns those) and does NOT send emails (NotificationService handles that).`]

**Data flow** (for the most representative user action):

[TODO: trace one user action from input → durable storage → response. Example: "User submits feedback → POST /api/feedback → FeedbackController validates schema → INSERT into Postgres `feedback` table → returns 201 with row id. No async fan-out."]

**Failure modes**:

- [TODO: for each entry in External Dependencies, state degradation behavior. Example: `Postgres unavailable → write endpoints return 503; reads served from in-process cache for 60s; alert fires via Sentry.`]

## Stack

{languages + frameworks}

## External Dependencies

- [TODO: list every external system the project talks to (databases, queues, third-party APIs, LLM providers, payment gateways, email providers). One bullet per dependency with version when relevant. Example: `Postgres 16 (primary store) · Redis 7 (rate-limit counters, sessions) · OpenAI API (LLM inference) · Stripe (billing).`]

## Methodology: Missions Architecture

This project follows **Missions Architecture** (orchestrator + workers + validators, externalized state, two-level TDD). See `~/.claude/projects/-Users-galileu-dev/memory/reference_missions_architecture.md` for the full methodology and lessons learned.

### Mission state

Each feature is self-contained in `docs/specs/<slug>/`:

```
docs/specs/<slug>/
├── spec.md                      # technical spec
├── design-prompt.md             # optional
├── designs/                     # screenshots
└── mission/
    ├── validation-contract.md   # black-box assertions
    ├── features.json            # task decomposition
    ├── knowledge-base.md        # decisions and learnings
    └── runs/                    # critic + worker + validator reports
```

### Workflow

1. **Spec** (skill `mission-spec`): feature idea → `docs/specs/<slug>/` with spec + assertions + features.
2. **Critic** (skill `mission-critic`): gates quality before implementation.
3. **Worker** (skill `mission-worker`): implements with TDD.
4. **Validator** (skill `mission-validator`): black-box, MUST NOT read Worker output.
5. If FAIL → **Refinement** (skill `mission-refinement`) generates fix features.

## Inviolable rules

[TODO: fill in domain-specific inviolables. Examples:
- Privacy: sensitive data encrypted at rest.
- Compliance: LGPD/GDPR if applicable.
- Tone: no emoji, no hype, short sentences (adjust per product).
- Audit: destructive actions logged to audit log.
]

## Roadmap (quick reference)

[TODO: list phases F00 → Fxx once defined in features.json]

## Hosting

{hosting target}
```

### `README.md`

Greenfield (full file). Brownfield (append the `## Methodology` section only):

```markdown
# {Project name}

{One-sentence description}

## Stack

{languages + frameworks}

## Getting started

```bash
[TODO: add setup commands once stack is final]
```

## Methodology

This project follows **Missions Architecture**.

- `CLAUDE.md` — rules for the orchestrator (Claude Code).
- `docs/specs/<slug>/spec.md` — technical spec per feature.
- `docs/specs/<slug>/mission/` — validation contract, features, knowledge base, and run logs per feature.

For visual tracking: [Mission Dashboard](http://localhost:3010) — add this project's path.

## Dashboard

```bash
cd ~/dev/mission-dashboard
npm run dev
# add this project's absolute path via the wizard
```
```

### `.gitignore`

Greenfield (Node-default if Node detected, otherwise generic):

```
node_modules
.next
dist
build
.DS_Store
.env
.env.local
*.log
.cache
data/
.audit/
.dashboard/
```

Brownfield: APPEND only missing lines. Always diff before writing.

---

## After creation — open with the build prompt

Run the mechanical critic silently first to confirm structural soundness:

```bash
node ~/.claude/skills/mission-critic/checks/run-mechanical.mjs --project .
```

If any `[M-*]` fail → report them and stop. Don't move to the build prompt until structure is clean.

If all pass, output a single concise message and end with the build prompt:

```
Mission structure created in {project}/  ·  15/15 mechanical checks pass.

What are we building today? Paste your ticket (Jira / Linear / GitHub issue / plain description) and I'll propose the validation-contract assertions + feature breakdown.
```

That's the whole "next steps" output. No bullet lists, no setup checklists. The build prompt is the call-to-action — once the user pastes a ticket, the normal orchestrator flow takes over.

**Reference (for the user to find later, NOT in the immediate output)**: dashboard registration (`cd ~/dev/mission-dashboard && npm run dev`), LLM critic phases (`run mission critic phase A|B|C`), and sub-agent file paths live in `CLAUDE.md` under the Methodology section. Pointing them out upfront is noise.

---

## Domain personalization

If the user mentions a regulated/clinical domain, BEFORE writing files, ask whether to include domain-specific defaults in CLAUDE.md:

- **Mental health / clinical**: sober tone (no emoji, no hype), trend charts only for professionals (not patients), neutral colors (no red/yellow/green traffic-light), mandatory human review on AI inferences, audit log of deletions without PII.
- **Finance**: strong idempotency on transactions, full audit, mandatory soft-delete.
- **Child education**: COPPA compliance, no data collection without parental consent, warm tone.
- **Legal**: full traceability, contract versioning.

These defaults go under "Inviolable rules" in CLAUDE.md.

---

## Antipatterns to avoid

- ❌ Skipping mode detection (running greenfield procedure on an existing repo → data loss).
- ❌ Overwriting `CLAUDE.md`/`README.md`/`.gitignore` in brownfield mode (always append, never replace).
- ❌ Re-initializing if `docs/specs/*/mission/features.json` already exists (ABORT and tell user).
- ❌ Re-asking for stack/owner/hosting in brownfield mode when they are detectable.
- ❌ Skipping the inventory step in brownfield (loses existing-behavior signal that should seed the contract).
- ❌ Creating files outside cwd (always relative).
- ❌ Adding features to `features.json` beyond the agreed initial set (scope comes later via orchestrator).
- ❌ Documenting decisions that haven't actually been made.
