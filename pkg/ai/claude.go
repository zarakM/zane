package ai

// We call the Anthropic API directly using Go's standard net/http package.
// No special SDK needed — the API is just JSON over HTTPS.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"kubectl-ai/pkg/k8s"
)

const (
	claudeAPIURL = "https://api.anthropic.com/v1/messages"
	claudeModel  = "claude-sonnet-4-20250514"
)

// ClaudeClient holds the API key and the HTTP client.
// http.Client is safe for concurrent use and should be reused — not created per request.
type ClaudeClient struct {
	apiKey     string
	httpClient *http.Client
}

// These structs mirror the Anthropic API's JSON shape.
// json:"field_name" tells Go's JSON encoder/decoder what key name to use.
// omitempty means the field is skipped if empty (useful for optional fields).
type claudeRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Stream    bool      `json:"stream"`
	System    string    `json:"system"`
	Messages  []message `json:"messages"`
}

// streamEvent mirrors the SSE JSON payloads the Anthropic streaming API sends.
// Only the fields we act on are decoded; unknown fields are silently ignored.
type streamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type string `json:"type"` // "text_delta" for token chunks
		Text string `json:"text"`
	} `json:"delta"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}


func NewClaudeClient(apiKey string) *ClaudeClient {
	return &ClaudeClient{
		apiKey:     apiKey,
		httpClient: &http.Client{},
	}
}

// Diagnose streams the diagnosis directly to out, printing tokens as they arrive.
// The caller is responsible for any surrounding formatting (separators, newlines).
func (c *ClaudeClient) Diagnose(ctx context.Context, data *k8s.DiagnosticData, out io.Writer) error {
	return c.streamTo(ctx, claudeRequest{
		Model:     claudeModel,
		MaxTokens: 1024,
		Stream:    true,
		System:    systemPrompt(),
		Messages:  []message{{Role: "user", Content: buildPrompt(data)}},
	}, out)
}

// streamTo is the shared SSE streaming implementation used by all Diagnose* methods.
// The Anthropic streaming API sends Server-Sent Events — lines prefixed with "data: "
// containing JSON payloads. We decode each chunk and write text deltas to out immediately,
// so the user sees output token by token rather than waiting for the full response.
func (c *ClaudeClient) streamTo(ctx context.Context, reqBody claudeRequest, out io.Writer) error {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeAPIURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("API request failed: %w", err)
	}
	// defer runs when the surrounding function returns — critical for closing
	// the response body to avoid leaking the HTTP connection.
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	// bufio.Scanner reads one line at a time without buffering the full body.
	// This is what makes streaming work — we process each SSE event as it arrives.
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()

		// SSE format: payload lines start with "data: "; blank lines separate events.
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}

		var event streamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			// Skip malformed events — the stream may include keepalive pings
			// or future event types we don't know about yet.
			continue
		}

		switch event.Type {
		case "content_block_delta":
			if event.Delta.Type == "text_delta" {
				// Write without buffering so the terminal updates immediately.
				fmt.Fprint(out, event.Delta.Text)
			}
		case "error":
			return fmt.Errorf("Claude API error (%s): %s", event.Error.Type, event.Error.Message)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading stream: %w", err)
	}

	// Streaming output doesn't guarantee a trailing newline — add one so the
	// caller's closing separator lands on its own line.
	fmt.Fprintln(out)
	return nil
}

// DiagnosePending streams the pending-pod diagnosis to out using the scheduling-focused prompt.
func (c *ClaudeClient) DiagnosePending(ctx context.Context, data *k8s.PendingDiagnosticData, out io.Writer) error {
	return c.streamTo(ctx, claudeRequest{
		Model:     claudeModel,
		MaxTokens: 1024,
		Stream:    true,
		System:    pendingSystemPrompt(),
		Messages:  []message{{Role: "user", Content: buildPendingPrompt(data)}},
	}, out)
}

// DiagnoseRollout streams the stuck-rollout diagnosis to out using the rollout-focused prompt.
func (c *ClaudeClient) DiagnoseRollout(ctx context.Context, data *k8s.RolloutDiagnosticData, out io.Writer) error {
	return c.streamTo(ctx, claudeRequest{
		Model:     claudeModel,
		MaxTokens: 1024,
		Stream:    true,
		System:    rolloutSystemPrompt(),
		Messages:  []message{{Role: "user", Content: buildRolloutPrompt(data)}},
	}, out)
}

// pendingSystemPrompt is tuned for scheduling failures, not runtime crashes.
// The evidence that matters is totally different: node taints, quota limits, PVC binding.
func pendingSystemPrompt() string {
	return `You are an expert Kubernetes scheduler and SRE with 10+ years of production experience.

You will be given diagnostic data for a pod stuck in Pending state: the pod spec, scheduler events, node capacity and taints, namespace resource quotas, and PVC binding status.

Your job is to identify exactly why the scheduler cannot place this pod and tell the engineer how to fix it.

Always respond in this exact format — no preamble, start immediately with the first heading:

## 🔴 Root Cause
One clear sentence. Why can't the scheduler place this pod?

## 📊 Confidence
High / Medium / Low — one sentence explaining why.

## 🔍 Evidence
2–3 bullet points directly quoted or referenced from the data that support your diagnosis.

## 🔢 Probable Causes (ranked by likelihood)
1. Most likely: brief explanation
2. Second possibility: brief explanation
3. Third possibility (only if genuinely plausible)

## ⚡ Next Command
The single most useful kubectl command to run right now to confirm the root cause.
Format: ` + "`kubectl ...`" + `

## 🔧 Fix
Concrete action to resolve. Be specific:
- If nodes lack capacity, show the resource request values to reduce or the node pool to scale.
- If a taint blocks scheduling, show the exact toleration YAML to add to the pod spec.
- If a quota is exhausted, show which resource is over limit and how to raise it.
- If a PVC is unbound, explain why and how to fix the StorageClass or provisioner.

Do not hedge. Do not say "it could be many things." Pick the most likely cause and commit to it.`
}

// buildPendingPrompt constructs the user message for a pending pod diagnosis.
func buildPendingPrompt(data *k8s.PendingDiagnosticData) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Diagnose this pending Kubernetes pod: %s (namespace: %s)\n\n", data.PodName, data.Namespace))

	sb.WriteString("## Pod Spec\n```\n")
	sb.WriteString(data.PodSpec)
	sb.WriteString("```\n\n")

	// Events are the highest-signal input for pending pods — the scheduler writes
	// the exact reason it couldn't place the pod here.
	sb.WriteString("## Kubernetes Events\n```\n")
	sb.WriteString(data.Events)
	sb.WriteString("\n```\n\n")

	sb.WriteString("## Node Summary (capacity, allocatable, taints)\n```\n")
	sb.WriteString(data.NodeSummary)
	sb.WriteString("```\n\n")

	sb.WriteString("## Namespace Resource Quotas\n```\n")
	sb.WriteString(data.QuotaSummary)
	sb.WriteString("```\n\n")

	sb.WriteString("## PersistentVolumeClaim Status\n```\n")
	sb.WriteString(data.PVCSummary)
	sb.WriteString("\n```\n\n")

	sb.WriteString("Provide a structured diagnosis following the format in your instructions.")

	return sb.String()
}

// systemPrompt defines Claude's role and the exact output format we want.
// Getting this prompt right is the core product work — the Go code is just plumbing.
func systemPrompt() string {
	return `You are an expert Kubernetes SRE with 10+ years of production incident experience.

You will be given diagnostic data from a failing Kubernetes pod: the pod spec, container statuses, recent events, and logs.

Your job is to diagnose the issue and tell the engineer exactly what is wrong and how to fix it.

Always respond in this exact format — no preamble, start immediately with the first heading:

## 🔴 Root Cause
One clear sentence. What is broken right now.

## 📊 Confidence
High / Medium / Low — one sentence explaining why.

## 🔍 Evidence
2–3 bullet points directly quoted or referenced from the data that support your diagnosis.

## 🔢 Probable Causes (ranked by likelihood)
1. Most likely: brief explanation
2. Second possibility: brief explanation
3. Third possibility (only if genuinely plausible)

## ⚡ Next Command
The single most useful kubectl command to run right now to confirm the root cause.
Format: ` + "`kubectl ...`" + `

## 🔧 Fix
Concrete action to resolve. Be specific:
- If it's a config change, show the exact YAML field and value.
- If it's a command, show the exact command.
- If it's a resource limit issue, show the corrected values.

Do not hedge. Do not say "it could be many things." Pick the most likely cause and commit to it.`
}

// buildPrompt constructs the user message with all the diagnostic context.
// The quality of this prompt directly determines the quality of the diagnosis.
func buildPrompt(data *k8s.DiagnosticData) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Diagnose this failing Kubernetes pod: %s (namespace: %s)\n\n", data.PodName, data.Namespace))

	// Pod spec
	sb.WriteString("## Pod Spec\n```\n")
	sb.WriteString(data.PodSpec)
	sb.WriteString("```\n\n")

	// Container runtime status
	if len(data.Containers) > 0 {
		sb.WriteString("## Container Status\n")
		for _, cs := range data.Containers {
			sb.WriteString(fmt.Sprintf("- **%s**: %s\n", cs.Name, cs.State))
			sb.WriteString(fmt.Sprintf("  Restarts: %d | Ready: %v\n", cs.RestartCount, cs.Ready))
			if cs.LastState != "" {
				sb.WriteString(fmt.Sprintf("  Last crash: %s\n", cs.LastState))
			}
		}
		sb.WriteString("\n")
	}

	// Events (these are gold — often contain the most direct explanation)
	sb.WriteString("## Kubernetes Events\n```\n")
	sb.WriteString(data.Events)
	sb.WriteString("\n```\n\n")

	// Logs
	if data.Logs != "" {
		sb.WriteString(fmt.Sprintf("## Container Logs (last %d lines)\n```\n", data.LogLineCount))
		sb.WriteString(data.Logs)
		sb.WriteString("\n```\n\n")
	} else {
		sb.WriteString("## Container Logs\nNo logs available (pod may not have started).\n\n")
	}

	sb.WriteString("Provide a structured diagnosis following the format in your instructions.")

	return sb.String()
}

// rolloutSystemPrompt is tuned for stuck Deployment rollouts. The highest-signal
// fields here are the Deployment's Progressing / Available conditions and the
// Worst Pod's container state — call those out explicitly so Claude anchors on them.
func rolloutSystemPrompt() string {
	return `You are an expert Kubernetes SRE with 10+ years of production experience diagnosing stuck Deployment rollouts.

You will be given diagnostic data for a Deployment whose rollout is stuck or progressing slowly: the deployment spec, status, conditions, owned ReplicaSets, the pods they own, the worst pod's spec and logs, combined events, and any matching PodDisruptionBudgets.

The most common causes of a stuck rollout, ranked by frequency:
1. New pods failing readiness — readiness probe never passes; new ReplicaSet sits at ready=0.
2. ImagePullBackOff / ErrImagePull on the new image tag — typo, missing imagePullSecret, registry outage.
3. ProgressDeadlineExceeded — explicit "Progressing=False" condition with reason=ProgressDeadlineExceeded.
4. CrashLoopBackOff in new pods — config / dep / migration mismatch in the new image.
5. PodDisruptionBudget blocking termination of old pods (disruptionsAllowed=0 with currentHealthy at the floor).
6. maxUnavailable=0 + a single broken new pod — the surge can never proceed.
7. Resource pressure — new pods cannot schedule (rare; usually surfaces as Pending pods, see scheduler events).

The Deployment's .status.conditions (Progressing, Available) is the highest-signal data here — quote it directly.
The Worst Pod logs are your second-highest signal — that pod was selected because it's the most-broken replica.

Always respond in this exact format — no preamble, start immediately with the first heading:

## 🔴 Root Cause
One clear sentence. Why is this rollout stuck?

## 📊 Confidence
High / Medium / Low — one sentence explaining why.

## 🔍 Evidence
2–3 bullet points directly quoted or referenced from the data that support your diagnosis.
Prefer quotes from .status.conditions and the worst pod's logs / container state.

## 🔢 Probable Causes (ranked by likelihood)
1. Most likely: brief explanation
2. Second possibility: brief explanation
3. Third possibility (only if genuinely plausible)

## ⚡ Next Command
The single most useful kubectl command to run right now to confirm the root cause.
Format: ` + "`kubectl ...`" + `

## 🔧 Fix
Concrete action to resolve. Be specific:
- If a readiness probe is failing, name the probe and show what to change (path, port, initialDelaySeconds).
- If it's an image pull issue, show the exact image string to fix or imagePullSecret to add.
- If a PDB is blocking, show how to relax it (or which old pod to evict).
- If maxUnavailable is the issue, show the strategy YAML change.

Do not hedge. Do not say "it could be many things." Pick the most likely cause and commit to it.`
}

// buildRolloutPrompt constructs the user message for a stuck-rollout diagnosis.
// Section ordering matters: status/conditions first (the canonical "stuck" signal),
// then progressively more detail, ending with PDBs (the often-overlooked blocker).
func buildRolloutPrompt(data *k8s.RolloutDiagnosticData) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Diagnose this stuck Kubernetes Deployment rollout: %s (namespace: %s)\n\n",
		data.DeploymentName, data.Namespace))

	sb.WriteString("## Deployment Spec\n```\n")
	sb.WriteString(data.DeploymentSpec)
	sb.WriteString("```\n\n")

	sb.WriteString("## Deployment Status & Conditions\n```\n")
	sb.WriteString(data.Status)
	sb.WriteString("```\n\n")

	sb.WriteString("## ReplicaSets (newest first)\n```\n")
	sb.WriteString(data.ReplicaSets)
	sb.WriteString("```\n\n")

	sb.WriteString(fmt.Sprintf("## Pods (%d total)\n```\n", data.PodCount))
	sb.WriteString(data.PodSummary)
	sb.WriteString("```\n\n")

	if data.WorstPodName != "" {
		sb.WriteString(fmt.Sprintf("## Worst Pod Spec — %s\n```\n", data.WorstPodName))
		sb.WriteString(data.WorstPodSpec)
		sb.WriteString("```\n\n")

		if data.WorstPodLogs != "" {
			sb.WriteString(fmt.Sprintf("## Worst Pod Logs — %s\n```\n", data.WorstPodName))
			sb.WriteString(data.WorstPodLogs)
			sb.WriteString("\n```\n\n")
		} else {
			sb.WriteString("## Worst Pod Logs\nNo logs available (container may not have started).\n\n")
		}
	}

	sb.WriteString("## Events (Deployment + ReplicaSets + Pods, newest first)\n```\n")
	sb.WriteString(data.Events)
	sb.WriteString("\n```\n\n")

	sb.WriteString("## Matching PodDisruptionBudgets\n```\n")
	sb.WriteString(data.PDBs)
	sb.WriteString("\n```\n\n")

	sb.WriteString("Provide a structured diagnosis following the format in your instructions.")

	return sb.String()
}

// ---------------------------------------------------------------------------
// Co-Pilot path — router + free-form answer.
// `Diagnose*` is for the rigid `diagnose`/`rollout` commands.
// `Route` + `Answer*` is for the free-form `ask` command, where the user
// types a natural-language question and we route to the matching strategy.
// ---------------------------------------------------------------------------

// RouteDecision is what Route returns — which strategy to use and the target
// resource name. Kind is one of "pod", "deployment", "generic".
type RouteDecision struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// completeResponse mirrors the non-streaming Anthropic /v1/messages JSON body.
// We only decode the fields we read; unknown fields are ignored.
type completeResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// complete is the non-streaming companion to streamTo. It returns the full
// assistant text. Used by Route() where we want a small JSON answer back rather
// than a streamed multi-paragraph response.
func (c *ClaudeClient) complete(ctx context.Context, reqBody claudeRequest) (string, error) {
	reqBody.Stream = false

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to build request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeAPIURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var parsed completeResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("could not decode response: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("Claude API error (%s): %s", parsed.Error.Type, parsed.Error.Message)
	}

	var sb strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return sb.String(), nil
}

// Route asks Claude to classify the user's free-form question against a small
// inventory of pod and deployment names in the chosen namespace. Returns a
// {Kind, Name} pair the caller dispatches on.
//
// The router is intentionally conservative: if it can't confidently identify a
// specific resource it returns Kind="generic" so the caller can prompt the user
// rather than guessing wrong.
func (c *ClaudeClient) Route(ctx context.Context, question, namespace, inventory string) (RouteDecision, error) {
	userPrompt := fmt.Sprintf(`Question: %s

Namespace: %s

Inventory:
%s

Return your decision as JSON.`, question, namespace, inventory)

	raw, err := c.complete(ctx, claudeRequest{
		Model:     claudeModel,
		MaxTokens: 200,
		System:    routerSystemPrompt(),
		Messages:  []message{{Role: "user", Content: userPrompt}},
	})
	if err != nil {
		return RouteDecision{}, err
	}

	// The model is asked to return JSON only, but it sometimes wraps in code fences.
	// Be tolerant: find the first { and the last }.
	first := strings.Index(raw, "{")
	last := strings.LastIndex(raw, "}")
	if first < 0 || last <= first {
		return RouteDecision{}, fmt.Errorf("router returned no JSON object: %q", raw)
	}

	var d RouteDecision
	if err := json.Unmarshal([]byte(raw[first:last+1]), &d); err != nil {
		return RouteDecision{}, fmt.Errorf("could not parse router decision: %w (raw=%q)", err, raw)
	}
	return d, nil
}

// routerSystemPrompt instructs Claude to map a free-form question to one of the
// supported strategies. The valid Kinds match the gather strategies the binary
// has today; "generic" is the fallback for questions that don't name a specific
// resource (e.g. "what's healthy in this namespace?").
func routerSystemPrompt() string {
	return `You are a routing classifier inside a Kubernetes co-pilot CLI.

Given the user's natural-language question, the namespace they are operating in, and an inventory of pod and deployment names in that namespace, decide which diagnostic strategy fits.

Return ONLY a single JSON object with exactly two fields:
  {"kind": "pod" | "deployment" | "generic", "name": "<resource name or empty string>"}

Rules:
- If the question mentions or strongly implies a specific pod from the inventory, return kind="pod" with that pod name.
- If it mentions or strongly implies a specific deployment from the inventory (rollout, replicas, "deployment X is stuck"), return kind="deployment" with that deployment name.
- If the question references a name that resembles a pod (often deployment-name plus a hash suffix like "-7d8f9c"), prefer kind="pod" and use the literal name from the inventory.
- If the question is generic ("what's wrong in this namespace?", "are my workloads healthy?") or you cannot match a resource confidently, return kind="generic" with name="".
- Do NOT invent resource names that are not in the inventory. If the user names a resource not in the inventory, still return it under kind="pod" or kind="deployment" using their spelling — the caller will surface a not-found error.

Output JSON only. No prose, no code fences.`
}

// AnswerCrash streams a free-form answer using crash diagnostic data.
// The Answer* family uses a conversational system prompt (not the rigid
// 6-section schema used by Diagnose*).
func (c *ClaudeClient) AnswerCrash(ctx context.Context, question string, data *k8s.DiagnosticData, out io.Writer) error {
	return c.streamTo(ctx, claudeRequest{
		Model:     claudeModel,
		MaxTokens: 1024,
		Stream:    true,
		System:    answerSystemPrompt(),
		Messages:  []message{{Role: "user", Content: buildAnswerCrashPrompt(question, data)}},
	}, out)
}

// AnswerPending streams a free-form answer using pending-pod diagnostic data.
func (c *ClaudeClient) AnswerPending(ctx context.Context, question string, data *k8s.PendingDiagnosticData, out io.Writer) error {
	return c.streamTo(ctx, claudeRequest{
		Model:     claudeModel,
		MaxTokens: 1024,
		Stream:    true,
		System:    answerSystemPrompt(),
		Messages:  []message{{Role: "user", Content: buildAnswerPendingPrompt(question, data)}},
	}, out)
}

// AnswerRollout streams a free-form answer using rollout diagnostic data.
func (c *ClaudeClient) AnswerRollout(ctx context.Context, question string, data *k8s.RolloutDiagnosticData, out io.Writer) error {
	return c.streamTo(ctx, claudeRequest{
		Model:     claudeModel,
		MaxTokens: 1024,
		Stream:    true,
		System:    answerSystemPrompt(),
		Messages:  []message{{Role: "user", Content: buildAnswerRolloutPrompt(question, data)}},
	}, out)
}

// answerSystemPrompt is shared by all Answer* methods. Unlike the Diagnose*
// prompts, it does not enforce a fixed 6-section schema — the question shape
// determines the answer shape. It does enforce evidence-citation discipline
// (no hand-waving) and a "Next Step" suffix when the question implies action.
func answerSystemPrompt() string {
	return `You are the user's Kubernetes operations co-pilot — concise, opinionated, evidence-driven.

You will be given a user question and structured cluster data relevant to that question. Answer the question directly using only that data.

Format rules:
- Lead with a one-sentence direct answer.
- Then 2–3 short bullet points of evidence drawn from the supplied data — quote exact values where possible (probe path, image tag, replica counts, exit code, event reason).
- If the question implies action (something is broken or needs fixing), end with a "Next step:" line containing exactly one concrete kubectl command or YAML change.
- If the data is insufficient to answer confidently, say so in one sentence and name the single piece of additional data that would resolve it.

Tone:
- Plain English. No hedging language ("it could be many things"). Pick the most likely explanation and commit.
- Do not echo the user's question text back verbatim. Refer to resources by their kind ("this pod", "the deployment") rather than re-typing names the user used.
- No preamble, no closing pleasantries. First sentence is the answer.`
}

// buildAnswerCrashPrompt prepends the user's question to the same diagnostic
// payload used by buildPrompt — the gathered data is the same; only the system
// prompt and the leading "Question:" line differ.
func buildAnswerCrashPrompt(question string, data *k8s.DiagnosticData) string {
	var sb strings.Builder
	sb.WriteString("Question: ")
	sb.WriteString(question)
	sb.WriteString("\n\n")
	sb.WriteString("---\n\n")
	sb.WriteString(buildPrompt(data))
	return sb.String()
}

// buildAnswerPendingPrompt is the pending-pod equivalent of buildAnswerCrashPrompt.
func buildAnswerPendingPrompt(question string, data *k8s.PendingDiagnosticData) string {
	var sb strings.Builder
	sb.WriteString("Question: ")
	sb.WriteString(question)
	sb.WriteString("\n\n")
	sb.WriteString("---\n\n")
	sb.WriteString(buildPendingPrompt(data))
	return sb.String()
}

// buildAnswerRolloutPrompt is the rollout equivalent of buildAnswerCrashPrompt.
func buildAnswerRolloutPrompt(question string, data *k8s.RolloutDiagnosticData) string {
	var sb strings.Builder
	sb.WriteString("Question: ")
	sb.WriteString(question)
	sb.WriteString("\n\n")
	sb.WriteString("---\n\n")
	sb.WriteString(buildRolloutPrompt(data))
	return sb.String()
}
