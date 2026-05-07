package main

// zanecli — conversational Kubernetes co-pilot.
//
// Phase 5 entry point: REPL with config wizard, agent loop, write tools
// gated by pkg/safety, and optional persistent conversation history.
// Phase 6 polish (ANSI colors, error handling) is the remaining work.

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"zanecli/pkg/agent"
	"zanecli/pkg/config"
	"zanecli/pkg/history"
	"zanecli/pkg/k8s"
	"zanecli/pkg/tools"
	"zanecli/pkg/ui"
)

func main() {
	// Per-invocation auto-exec override. --auto and --no-auto are mutually
	// exclusive; if neither is set, the saved config value is used.
	autoFlag := flag.Bool("auto", false, "enable auto-exec for this session (overrides config)")
	noAutoFlag := flag.Bool("no-auto", false, "force confirm-everything for this session (overrides config)")
	flag.Parse()
	if *autoFlag && *noAutoFlag {
		fatalf("--auto and --no-auto are mutually exclusive")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Trap ⌃C: first signal cancels the in-flight agent step (if any);
	// second signal exits.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Fprintln(os.Stderr, "\n(interrupting; press ⌃C again to exit)")
		cancel()
		<-sigs
		os.Exit(130)
	}()

	cfg, err := config.LoadOrWizard(os.Stdin, os.Stdout)
	if err != nil {
		fatalf("config error: %v\n\nIf the file at ~/.zanecli/config.json is corrupt, delete it and re-run.", err)
	}

	// CLI flags override the saved config value for this invocation only —
	// they do not persist back to ~/.zanecli/config.json.
	if *autoFlag {
		cfg.AutoExec = true
	}
	if *noAutoFlag {
		cfg.AutoExec = false
	}

	client, err := k8s.NewClient(cfg.KubeconfigPath)
	if err != nil {
		fatalf("could not connect to cluster: %v\n\nIs your kubeconfig at %s valid? Try: kubectl --kubeconfig %s get pods", err, cfg.KubeconfigPath, cfg.KubeconfigPath)
	}

	// Single shared scanner for both REPL input and write-confirmation
	// prompts — they must not compete for stdin.
	scanner := bufio.NewScanner(os.Stdin)
	confirmer := &stdinConfirmer{scanner: scanner}

	registry := tools.NewRegistry(client)
	sess := agent.NewSession(cfg, client, registry, confirmer)

	// History is opt-in. Open the writer only if the user enabled it.
	var writer *history.Writer
	if cfg.HistoryEnabled {
		writer, err = history.OpenWriter()
		if err != nil {
			fmt.Fprintf(os.Stderr, "history disabled: %v\n", err)
			writer = nil
		}
		if writer != nil {
			defer writer.Close()
		}
	}

	fmt.Printf("%szanecli%s — your Kubernetes co-pilot\n", ui.Bold+ui.Cyan, ui.Reset)
	fmt.Printf("Cluster: %s%s%s\n", ui.Dim, abbreviateServerURL(client.ServerURL()), ui.Reset)
	printAutoExecStatus(cfg.AutoExec)

	// Offer to resume a prior session if history is on and a previous file exists.
	if cfg.HistoryEnabled {
		offerResume(sess, scanner)
	}

	fmt.Println("Type your question, or 'exit' to quit. Use /clear to reset the conversation; /auto and /no-auto toggle auto-fix.")
	fmt.Println()

	// Track the persisted prefix so we only append new messages after each Step.
	persistedPrefix := len(sess.Messages())

	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		switch line {
		case "":
			continue
		case "exit", "quit":
			return
		case "/clear":
			sess.Clear()
			persistedPrefix = 0
			fmt.Println("(conversation cleared)")
			continue
		case "/auto":
			cfg.AutoExec = true
			printAutoExecStatus(cfg.AutoExec)
			continue
		case "/no-auto":
			cfg.AutoExec = false
			printAutoExecStatus(cfg.AutoExec)
			continue
		}

		if err := sess.Step(ctx, line, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "%sagent error:%s %v\n", ui.Red, ui.Reset, err)
		}
		fmt.Println()

		// Persist any new messages this Step produced (user input + assistant
		// turns + tool results). Best-effort; a write failure doesn't break the chat.
		if writer != nil {
			msgs := sess.Messages()
			for i := persistedPrefix; i < len(msgs); i++ {
				if werr := writer.Append(msgs[i]); werr != nil {
					fmt.Fprintf(os.Stderr, "history write failed: %v\n", werr)
					break
				}
			}
			persistedPrefix = len(msgs)
		}
	}
}

// offerResume looks for the most recent prior session and asks the user if
// they want to load it as context. Always non-blocking: any error or "no"
// answer drops through to a fresh session.
func offerResume(sess *agent.Session, scanner *bufio.Scanner) {
	prior, err := history.LoadLatest()
	if err != nil || prior == nil || len(prior.Messages) == 0 {
		return
	}
	fmt.Printf("Resume last session (%s)? [y/N]: ", prior.Summary())
	if !scanner.Scan() {
		return
	}
	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
	if answer == "y" || answer == "yes" {
		sess.LoadMessages(prior.Messages)
		fmt.Printf("Resumed %d messages from %s\n", len(prior.Messages), prior.Path)
	}
}

// stdinConfirmer asks yes/no on the shared REPL scanner. Lives here in main
// so the agent package stays free of stdin/terminal concerns.
type stdinConfirmer struct {
	scanner *bufio.Scanner
}

func (c *stdinConfirmer) AskYesNo(prompt string) bool {
	fmt.Print(prompt)
	if !c.scanner.Scan() {
		return false
	}
	s := strings.ToLower(strings.TrimSpace(c.scanner.Text()))
	return s == "y" || s == "yes"
}

// fatalf prints a colored error and exits with code 1. Used at startup
// when there's nothing usable to fall back to.
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s✗%s "+format+"\n", append([]any{ui.Red, ui.Reset}, args...)...)
	os.Exit(1)
}

// printAutoExecStatus shows the session's current auto-exec posture so it
// stays visible across context switches. Loud (yellow) when ON, quiet (dim)
// when off — auto-exec is the high-stakes setting and should not fade into
// the background after the user enabled it.
func printAutoExecStatus(on bool) {
	if on {
		fmt.Printf("%s[auto-exec: ON — whitelisted writes may run without prompting]%s\n", ui.Yellow, ui.Reset)
	} else {
		fmt.Printf("%s[auto-exec: off — every write asks first]%s\n", ui.Dim, ui.Reset)
	}
}

// abbreviateServerURL turns "https://prod-east.cluster.local:6443" into
// something short enough for the banner without leaking full URLs.
func abbreviateServerURL(s string) string {
	if s == "" {
		return "(no API server resolved)"
	}
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	return s
}
