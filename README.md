# kubectl-ai — AI Kubernetes Co-Pilot

> Ask your cluster anything. Routes to the right diagnostic, answers in plain English.

## What it does

Three entry points share the same gather + Claude pipeline:

- **`ask "<question>"`** — free-form natural-language entry point. Routes the question to the right strategy (pod / deployment) automatically and answers conversationally with cited evidence.
- **`diagnose <pod>`** — focused diagnostic for a broken pod. Detects the failure type automatically:
  - **Crashing pod** (CrashLoopBackOff, OOMKilled) — correlates logs, events, and resource limits
  - **Pending pod** — correlates node capacity, taints, quotas, and PVC binding status
- **`rollout <deployment>`** — focused diagnostic for a stuck Deployment rollout. Picks the worst replica, gathers its logs, and reports.

All three stream the answer token-by-token. The focused commands return a structured diagnosis (root cause, confidence, evidence, next command, exact fix); `ask` returns a direct answer with cited evidence and an optional next step.

## Requirements

- Go 1.23+
- A kubeconfig with access to your cluster
- An [Anthropic API key](https://console.anthropic.com/)

## Install

```bash
go install github.com/zarakm/aik8scopilot@latest
```

Or build from source:

```bash
git clone https://github.com/zarakm/aik8scopilot
cd aik8scopilot
go build -o kubectl-ai .
cp kubectl-ai /usr/local/bin/kubectl-ai
```

Cross-compile for Linux (e.g. a jump server):

```bash
GOOS=linux GOARCH=amd64 go build -o kubectl-ai-linux .
```

## Usage

```bash
export ANTHROPIC_API_KEY=your-key

# Free-form: ask anything, the router picks the right strategy
kubectl-ai ask "why is checkout-api failing?" -n production
kubectl-ai ask "is my web deployment healthy?"

# Focused: works on any broken pod — auto-detects crash vs pending
kubectl-ai diagnose <pod-name> -n <namespace>

# Focused: stuck Deployment rollouts
kubectl-ai rollout <deployment-name> -n <namespace>

# Use a specific kubeconfig
kubectl-ai --kubeconfig ./my-kubeconfig ask "what's wrong here?" -n production

# Disable telemetry for a single run
kubectl-ai ask "..." --no-telemetry
```

## Telemetry

kubectl-ai collects anonymous usage data to improve diagnosis quality — error type, sanitized container states, event reasons, Claude's response, and a hashed cluster fingerprint. **No pod names, namespace names, env var values, or secret values are stored.**

To opt out, pass `--no-telemetry` on any run.

## Status

Early MVP — feedback welcome. Open an issue or DM on LinkedIn.
