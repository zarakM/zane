package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"kubectl-ai/pkg/ai"
	"kubectl-ai/pkg/k8s"
	"kubectl-ai/pkg/telemetry"
)

var askCmd = &cobra.Command{
	Use:   "ask <question>",
	Short: "Ask a free-form question about your cluster",
	Long: `Ask a natural-language question and let the co-pilot route it to the right
diagnostic strategy automatically.

The router decides whether the question is about a specific pod, a deployment,
or a generic namespace question, then runs the matching gather + answer flow.
Wrap the question in quotes so the shell passes it as a single argument.

Examples:
  kubectl-ai ask "why is checkout-api crashing?" -n production
  kubectl-ai ask "is my web deployment healthy?"
  kubectl-ai ask "what's wrong with the stuck-rollout deployment"

The ANTHROPIC_API_KEY environment variable must be set.`,

	Args: cobra.ExactArgs(1),
	RunE: runAsk,
}

func init() {
	rootCmd.AddCommand(askCmd)
	askCmd.Flags().IntP("lines", "l", 50, "Number of log lines to fetch when the question routes to a pod or deployment")
	askCmd.Flags().Bool("no-telemetry", false, "Disable anonymous usage telemetry for this run")
}

func runAsk(cmd *cobra.Command, args []string) error {
	question := args[0]

	namespace, _ := cmd.Flags().GetString("namespace")
	logLines, _ := cmd.Flags().GetInt("lines")
	kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
	noTelemetry, _ := cmd.Flags().GetBool("no-telemetry")

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("ANTHROPIC_API_KEY is not set\n\nRun: export ANTHROPIC_API_KEY=your-key-here")
	}

	ctx := context.Background()

	fmt.Printf("\n💬 Thinking about your question (namespace %q)...\n", namespace)

	client, err := k8s.NewClient(kubeconfig)
	if err != nil {
		return fmt.Errorf("could not connect to cluster: %w\n\nIs your kubeconfig set up? Try: kubectl get pods", err)
	}

	// Routing requires a small inventory: pod and deployment names in the namespace.
	// Without this anchor the LLM would have to guess names from the question alone.
	inventory, err := client.GatherNamespaceInventory(ctx, namespace)
	if err != nil {
		return fmt.Errorf("could not list namespace resources for routing: %w", err)
	}

	claudeClient := ai.NewClaudeClient(apiKey)

	decision, err := claudeClient.Route(ctx, question, namespace, inventory.Format())
	if err != nil {
		return fmt.Errorf("router failed: %w", err)
	}

	switch decision.Kind {
	case "pod":
		if decision.Name == "" {
			return fmt.Errorf("router returned kind=pod but no name; please rephrase or specify the pod explicitly")
		}
		return runAskOnPod(ctx, client, claudeClient, namespace, decision.Name, question, logLines, noTelemetry)
	case "deployment":
		if decision.Name == "" {
			return fmt.Errorf("router returned kind=deployment but no name; please rephrase or specify the deployment explicitly")
		}
		return runAskOnDeployment(ctx, client, claudeClient, namespace, decision.Name, question, logLines, noTelemetry)
	default:
		// "generic" or unrecognised — we don't have a namespace-wide gather yet.
		// Suggest a more concrete question so the user can continue.
		return fmt.Errorf("couldn't identify a specific pod or deployment in your question\n\nTry mentioning a resource name, e.g. kubectl-ai ask \"why is checkout-api failing\" -n %s\n\nResources visible in this namespace:\n%s", namespace, inventory.Format())
	}
}

// runAskOnPod handles the pod-routed branch: gathers crash or pending data
// (matching the existing diagnose path), streams a free-form answer, and logs
// telemetry through the same Log functions as `diagnose`. We do not introduce a
// separate "ask" incident_type — the underlying signal shape is identical.
func runAskOnPod(ctx context.Context, client *k8s.Client, claudeClient *ai.ClaudeClient,
	namespace, podName, question string, logLines int, noTelemetry bool) error {

	phase, err := client.GetPodPhase(ctx, namespace, podName)
	if err != nil {
		return fmt.Errorf("failed to fetch pod %q: %w", podName, err)
	}

	fmt.Printf("📍 Routing to pod %q (phase=%s)\n", podName, phase)
	fmt.Println("─────────────────────────────────────────────")

	var diagBuf bytes.Buffer
	out := io.MultiWriter(os.Stdout, &diagBuf)

	if phase == "Pending" {
		data, gerr := client.GatherPendingDiagnostics(ctx, namespace, podName)
		if gerr != nil {
			return fmt.Errorf("failed to gather pending pod data: %w", gerr)
		}
		if aerr := claudeClient.AnswerPending(ctx, question, data, out); aerr != nil {
			return fmt.Errorf("AI answer failed: %w", aerr)
		}
		fmt.Println("─────────────────────────────────────────────")
		if !noTelemetry {
			telemetry.LogPendingIncident(data, diagBuf.String(), client.ServerURL())
		}
		return nil
	}

	data, gerr := client.GatherDiagnostics(ctx, namespace, podName, logLines)
	if gerr != nil {
		return fmt.Errorf("failed to gather pod data: %w", gerr)
	}
	if aerr := claudeClient.AnswerCrash(ctx, question, data, out); aerr != nil {
		return fmt.Errorf("AI answer failed: %w", aerr)
	}
	fmt.Println("─────────────────────────────────────────────")
	if !noTelemetry {
		telemetry.LogCrashIncident(data, diagBuf.String(), client.ServerURL())
	}
	return nil
}

// runAskOnDeployment handles the deployment-routed branch.
func runAskOnDeployment(ctx context.Context, client *k8s.Client, claudeClient *ai.ClaudeClient,
	namespace, deploymentName, question string, logLines int, noTelemetry bool) error {

	fmt.Printf("📍 Routing to deployment %q\n", deploymentName)
	fmt.Println("─────────────────────────────────────────────")

	data, err := client.GatherRolloutDiagnostics(ctx, namespace, deploymentName, logLines)
	if err != nil {
		return fmt.Errorf("failed to gather rollout data: %w", err)
	}

	var diagBuf bytes.Buffer
	out := io.MultiWriter(os.Stdout, &diagBuf)

	if aerr := claudeClient.AnswerRollout(ctx, question, data, out); aerr != nil {
		return fmt.Errorf("AI answer failed: %w", aerr)
	}
	fmt.Println("─────────────────────────────────────────────")

	if !noTelemetry {
		telemetry.LogRolloutIncident(data, diagBuf.String(), client.ServerURL())
	}
	return nil
}
