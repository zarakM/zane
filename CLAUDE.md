# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is
A kubectl plugin (startup product): an AI Kubernetes Co-Pilot. The user can either run a focused diagnostic command (`diagnose`, `rollout`) or ask a free-form question (`ask`) and the binary routes to the right gather strategy and answers.

## Build and run
```bash
go mod tidy
go build -o kubectl-ai .
export ANTHROPIC_API_KEY=...
./kubectl-ai ask "<question>" -n <namespace>             # free-form: routes to pod / deployment automatically
./kubectl-ai diagnose <pod-name> -n <namespace>          # focused: auto-detects crash vs pending
./kubectl-ai rollout <deployment-name> -n <namespace>    # focused: stuck Deployment rollouts
```

Test fixtures in `testdata/`:
- `crashloop-pod.yaml` — pod that crashes immediately. Used by `diagnose` directly and by `ask` (router will pick this pod when asked about a crashing/restarting pod).
- `stuck-rollout.yaml` — Deployment with a permanently failing readiness probe (`maxUnavailable: 0`, `progressDeadlineSeconds: 60`). Used by `rollout` directly and by `ask` (router picks this deployment when asked about a stuck rollout).

Production build with telemetry baked in (required to activate Supabase logging):
```bash
GOOS=linux GOARCH=amd64 go build -ldflags "\
  -X kubectl-ai/pkg/telemetry.supabaseURL=https://yourproject.supabase.co \
  -X kubectl-ai/pkg/telemetry.supabaseKey=your-anon-key" \
  -o kubectl-ai-linux .
```

Dev override (skip recompile): set `SUPABASE_URL` + `SUPABASE_KEY` env vars — they take precedence over ldflags values.

## Architecture

### Request flow

Focused commands (`diagnose`, `rollout`):
```
cmd/<command>.go
  → k8s.NewClient(kubeconfig)                  # builds clientset from kubeconfig
  → client.Gather*Diagnostics(ctx, ...)        # all cluster API calls, returns structured data
  → io.MultiWriter(os.Stdout, &diagBuf)        # tees streaming output for telemetry capture
  → ai.NewClaudeClient(apiKey).Diagnose*(...)  # builds prompt, streams SSE response token-by-token
  → telemetry.LogIncident(...)                 # fire-and-forget goroutine POST to Supabase
```

Free-form `ask`:
```
cmd/ask.go
  → k8s.NewClient(kubeconfig)
  → client.GatherNamespaceInventory(ctx, ns)   # cheap: pod + deployment name lists for routing
  → ai.ClaudeClient.Route(question, inventory) # non-streaming JSON: {kind, name}
  → switch kind: pod → GatherDiagnostics / GatherPendingDiagnostics; deployment → GatherRolloutDiagnostics
  → ai.ClaudeClient.Answer*(question, data)    # tuned conversational prompt (not the rigid 6-section schema)
  → telemetry.Log{Crash,Pending,Rollout}Incident(...)  # reuses existing Log functions; no new incident_type
```

### Package responsibilities
- `cmd/` — orchestration only; no business logic. Three commands:
  - `diagnose.go` — focused pod diagnostic. Auto-detects pod phase (Pending vs other) and routes to the right gathering path internally; there is intentionally no separate `pending` subcommand. Pattern: gather → stream → (log).
  - `rollout.go` — focused stuck-rollout diagnostic. Same pattern.
  - `ask.go` — free-form NL entry point. Adds an inventory + route step before the standard gather → stream → (log): inventory → Claude.Route → dispatch to one of the gather strategies → Answer\* (instead of Diagnose\*) → log via the matching existing `Log*Incident` (no new incident_type).
- `pkg/k8s/client.go` — all `client-go` calls. Three diagnostic data types: `DiagnosticData` (crash), `PendingDiagnosticData` (scheduling), `RolloutDiagnosticData` (stuck deployments). `formatPodSpec` strips secret values. `formatRolloutPods` ranks pods CrashLoopBackOff > ImagePull > Waiting > NotReady > Ready and picks one "worst pod" so the prompt only carries logs for the most-broken replica.
- `pkg/ai/claude.go` — prompt construction + streaming. `streamTo()` is shared by all `Diagnose*` and `Answer*` methods. The `Diagnose*` family uses the rigid 6-section schema (Root Cause / Confidence / Evidence / Probable Causes / Next Command / Fix). The `Answer*` family (used by `ask`) uses a conversational prompt that adapts to the question. `Route()` is non-streaming and returns a JSON `{kind, name}` decision.
- `pkg/telemetry/logger.go` — silent background logging to Supabase. `supabaseURL`/`supabaseKey` are injected via `-ldflags`; env vars override for local dev. Never blocks the CLI. All three paths (`crash`, `pending`, `rollout`) log via `LogCrashIncident` / `LogPendingIncident` / `LogRolloutIncident`, sharing a single `postIncident` helper. `--no-telemetry` disables per-run on every command. Each path writes a row tagged with `incident_type` and a path-specific `signals` jsonb shape.

### Telemetry data model (Supabase `incidents` table)
Eight fields: `incident_type` (`crash` | `pending` | `rollout`), `error_type`, `signals` (jsonb — schema-flexible per `incident_type`), `diagnosis`, `confidence`, `cluster_id` (SHA256 of server URL, first 8 bytes), `model`, `created_at`. No pod names, namespace names, deployment names, env var values, image strings, or secret names are stored. Sanitization invariant: `pkg/telemetry/logger.go` reads only from the structured side-fields on the diagnostic structs (e.g. `EventReasons`, `SchedulerReason`, `ReplicaCounts`, `WorstPodContainers`), never from the formatted-string fields. The single allowed exception is `WorstPodLogs`, used as `log_tail` for crash and rollout paths.

## Scope — Co-Pilot product

The product is the **AI Kubernetes Operations Co-Pilot** as described in the original business plan: a free-form-question entry point on top of focused diagnostic strategies, with a roadmap toward write actions and additional domain surfaces (YAML, runbooks, cost).

**Currently in code (Path 2 step 1 — shipped):**
1. `diagnose <pod>` — auto-detects crash vs pending and runs the matching gather + diagnose prompt.
2. `rollout <deployment>` — stuck-rollout gather + diagnose prompt.
3. `ask "<question>" [-n ns]` — free-form NL entry point. Routes via Claude to pod / deployment / generic and runs an `Answer*` prompt (conversational, not the rigid 6-section schema).

**Roadmap (Path 2):**
- Step 2: write actions — `--apply` flag for safe remediations (image rollback, `rollout restart`, delete crashing pod). Dry-run by default. Refuse on production-pattern namespaces without `--force`.
- Step 3: a second domain surface — either YAML debugging (`explain -f`) or runbook retrieval. Picks up a second of the four plan domains.
- Step 4: Slack integration (`--slack-webhook`).
- Step 5: public GitHub launch / README polish.

**Out of scope (intentional):**
- StatefulSet / DaemonSet rollouts.
- Cluster-autoscaler reasoning.
- Web dashboard, SaaS billing, multi-cluster, RBAC. (These belong in the SaaS layer, not the CLI.)
- Adding LLM frameworks (LangChain, LlamaIndex) — direct HTTP to Anthropic remains the rule.

## Key decisions
- Direct HTTP to Anthropic — no SDK, intentional. Do not introduce LLM frameworks.
- Streaming via SSE (`bufio.Scanner` line-by-line) — `streamTo()` in `claude.go` is the single implementation.
- Anthropic key = user's own (`ANTHROPIC_API_KEY`). Supabase keys = yours, baked in via ldflags.
- Telemetry covers all three paths. Each path has its own structured side-fields on the diagnostic struct (added in `pkg/k8s/client.go`) which the logger reads — the formatted-string fields used in prompts are never touched by telemetry. Adding a new `incident_type` follows the same pattern: add side-fields, add a `Log*Incident` entry point with its own `*Signals` shape, gate at the call site on `--no-telemetry`.
- Rollout data gathering deliberately fetches logs for *only one* worst pod — picked by `formatRolloutPods` ranking — to keep the prompt within token budget even on 20-replica deployments.

## Code style
- Errors wrapped with `%w`, returned to `cmd/` layer for printing
- `context.Context` in every function that does I/O
- No global state — pass dependencies explicitly
- Comment the WHY, not the WHAT

## Do not
- Do not add a web server, database, or persistence layer
- Do not add authentication or multi-tenancy
- Do not change system prompts in `claude.go` without testing against real K8s errors. In particular, `answerSystemPrompt()` carries a directive forbidding Claude from echoing the user's question text verbatim — that directive is the soft mitigation against user-typed identifiers leaking via the `diagnosis` field in telemetry. Removing it widens the exposure surface.
- Do not store pod names, namespace names, env var values, or actual cluster URLs in telemetry. Note for `ask`: the user's question text is never persisted directly, but the streamed Claude response is (as `diagnosis`); the prompt directive above keeps Claude from re-emitting question-text identifiers in that response.
