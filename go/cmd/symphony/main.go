// Package main provides the Symphony orchestrator CLI entrypoint.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ling/symphony/internal/config"
	"github.com/ling/symphony/internal/orchestrator"
)

func main() {
	workflowPath := flag.String("workflow", "", "Path to WORKFLOW.md (default: ./WORKFLOW.md)")
	flag.Parse()

	// Use positional arg if provided
	if flag.NArg() > 0 {
		path := flag.Arg(0)
		workflowPath = &path
	}

	// Default to ./WORKFLOW.md
	if *workflowPath == "" {
		*workflowPath = "./WORKFLOW.md"
	}

	// Setup logger
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	logger := slog.New(handler)

	// Load workflow
	workflow, err := config.LoadWorkflow(*workflowPath)
	if err != nil {
		logger.Error("failed to load workflow", "path", *workflowPath, "error", err)
		os.Exit(1)
	}

	logger.Info("loaded workflow",
		"path", *workflowPath,
		"tracker", workflow.Config.Tracker.Kind,
		"project", workflow.Config.Tracker.ProjectSlug,
	)

	// Create orchestrator
	orch, err := orchestrator.New(workflow, logger)
	if err != nil {
		logger.Error("failed to create orchestrator", "error", err)
		os.Exit(1)
	}

	// Start orchestrator
	if err := orch.Start(); err != nil {
		logger.Error("failed to start orchestrator", "error", err)
		os.Exit(1)
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	logger.Info("received shutdown signal", "signal", sig)

	// Stop orchestrator
	orch.Stop()
	logger.Info("orchestrator stopped")

	fmt.Println("Symphony shutdown complete")
}
