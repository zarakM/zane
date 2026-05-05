package cmd

// Cobra is the same library kubectl itself uses for its CLI.
// Every command (diagnose, explain, etc.) gets registered to rootCmd.

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// rootCmd is the base command. Running `kubectl-ai` alone prints help.
var rootCmd = &cobra.Command{
	Use:   "kubectl-ai",
	Short: "AI Kubernetes Operations Co-Pilot",
	Long: `kubectl-ai is your AI Kubernetes operations co-pilot.

Ask a free-form question or run a focused diagnostic. The CLI collects pod
logs, events, deployment status, and cluster state, then uses an LLM to
answer in plain English.

Examples:
  kubectl-ai ask "why is checkout-api failing?" -n production
  kubectl-ai diagnose my-pod -n production
  kubectl-ai rollout my-deployment -n production`,
}

// Execute is called by main.go. It runs the CLI and handles any top-level error.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	// PersistentFlags are inherited by every sub-command (diagnose, explain, etc.)
	rootCmd.PersistentFlags().StringP("namespace", "n", "default", "Kubernetes namespace")
	rootCmd.PersistentFlags().StringP("kubeconfig", "", "", "Path to kubeconfig (defaults to ~/.kube/config)")
}
