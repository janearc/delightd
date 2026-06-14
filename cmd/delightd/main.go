package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"delightd/config"
	"delightd/pkg/backup"
	"delightd/pkg/exports"
	"delightd/pkg/metrics"
	"delightd/pkg/skills"
	"delightd/pkg/state"
	"delightd/pkg/watcher"
	"delightd/pkg/discovery"
	"delightd/pkg/traefik"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "execute without writing checkpoints to disk")
	immediate := flag.Bool("immediate", false, "execute an immediate evaluation on startup without waiting for the first interval tick")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if *dryRun {
		slog.Warn("DAEMON STARTED IN DRY RUN MODE - NO DESTRUCTIVE DISK WRITES WILL OCCUR")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.Load(ctx)
	if err != nil {
		slog.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}

	var mu sync.RWMutex
	machines := make(map[string]*state.Machine)
	for _, proj := range cfg.Projects {
		machines[proj.Name] = state.NewMachine(proj.Name)
	}

	slog.Info("configuration loaded successfully", "projects_count", len(cfg.Projects))

	workDir := os.ExpandEnv("$HOME/work")
	exportEngine := exports.NewEngine(workDir)
	var knownProjects []string
	for _, proj := range cfg.Projects {
		knownProjects = append(knownProjects, proj.Name)
	}

	if err := exportEngine.Sync(ctx, knownProjects, *dryRun); err != nil {
		slog.Error("initial export sync failed", "error", err)
	}

	skillAggregator := skills.NewAggregator(workDir)

	syncSkills := func() {
		if !cfg.System.AgentSkills.Enabled {
			return
		}
		if err := skillAggregator.ScanProjects(knownProjects); err != nil {
			slog.Error("failed to scan skills", "error", err)
		}

		// Handle CLI Generation
		for _, method := range cfg.System.AgentSkills.ExposeVia {
			if method == "cli" && !*dryRun {
				varBinDir := os.Getenv("DELIGHT_EXPORTS_BIN")
				if varBinDir == "" {
					home, _ := os.UserHomeDir()
					varBinDir = filepath.Join(home, "var", "bin")
				}
				if err := skills.GenerateCLIWrapper(varBinDir, skillAggregator.GetTools()); err != nil {
					slog.Error("failed to generate cli wrapper", "error", err)
				} else {
					slog.Info("regenerated fleet CLI wrapper", "tools_count", len(skillAggregator.GetTools()))
				}
			}
		}
	}

	// Initial skill sync
	syncSkills()

	go func() {
		slog.Info("starting periodic export sync engine")
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				exportEngine.Sync(ctx, knownProjects, *dryRun)
				syncSkills()
			}
		}
	}()

	go func() {
		slog.Info("starting periodic active control plane (LLM discovery)")
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		
		// Initial sync
		sources := discovery.DiscoverLocalLLMs(ctx, cfg)
		if err := traefik.SyncLLMRoutes(sources); err != nil {
			slog.Error("initial LLM traefik sync failed", "error", err)
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sources := discovery.DiscoverLocalLLMs(ctx, cfg)
				if err := traefik.SyncLLMRoutes(sources); err != nil {
					slog.Error("failed to sync LLM routes to traefik", "error", err)
				}
			}
		}
	}()

	for _, proj := range cfg.Projects {
		go func(p config.ProjectConfig) {
			if *immediate {
				slog.Info("executing immediate startup evaluation", "project", p.Name)
				mu.RLock()
				machine := machines[p.Name]
				mu.RUnlock()

				churn, err := watcher.HasChurn(ctx, p.Path)
				if err != nil {
					slog.Error("failed to poll git oracle on startup", "project", p.Name, "error", err)
				} else if churn {
					// Force straight to backing up for immediate evaluation
					machine.Transition(ctx, state.EventTriggerBackup)
				}
			}

			interval, err := time.ParseDuration(p.Backup.CheckInterval)
			if err != nil {
				slog.Error("invalid check_interval, defaulting to 15m", "project", p.Name)
				interval = 15 * time.Minute
			}

			pollTicker := time.NewTicker(interval)
			defer pollTicker.Stop()

			evalTicker := time.NewTicker(2 * time.Second)
			defer evalTicker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-pollTicker.C:
					mu.RLock()
					machine := machines[p.Name]
					mu.RUnlock()

					if machine.GetState() == state.StateFallow || machine.GetState() == state.StateMonitoring {
						metrics.Inc(fmt.Sprintf(`delightd_git_churn_checks_total{project="%s"}`, p.Name))
						churn, err := watcher.HasChurn(ctx, p.Path)
						if err != nil {
							slog.Error("failed to poll git oracle", "project", p.Name, "error", err)
							continue
						}

						if churn {
							if machine.GetState() == state.StateFallow {
								machine.Transition(ctx, state.EventChurnDetected)
							} else if machine.GetState() == state.StateMonitoring {
								machine.Transition(ctx, state.EventTriggerBackup)
							}
						}
					}

				case <-evalTicker.C:
					mu.RLock()
					machine := machines[p.Name]
					mu.RUnlock()

					if machine.GetState() == state.StateBackingUp {
						slog.Info("executing backup pipeline", "project", p.Name)

						archivePath, err := backup.CreateCheckpoint(ctx, p.Name, p.Path, cfg.System.Root+"/backups", p.Backup.Rotation.MaxArchives, *dryRun)
						if err != nil {
							metrics.Inc(fmt.Sprintf(`delightd_backup_failures_total{project="%s"}`, p.Name))
							slog.Error("backup pipeline failed", "project", p.Name, "error", err)
							machine.Transition(ctx, state.EventBackupFail)
						} else {
							metrics.Inc(fmt.Sprintf(`delightd_backup_success_total{project="%s"}`, p.Name))
							slog.Info("backup pipeline succeeded", "project", p.Name, "archive", archivePath)
							machine.Transition(ctx, state.EventBackupSuccess)
						}
					}

					if machine.GetState() == state.StateError && machine.CanRetry() {
						slog.Info("backoff period expired, triggering retry", "project", p.Name)
						machine.Transition(ctx, state.EventTriggerBackup)
					}
				}
			}
		}(proj)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"ok", "active_projects":%d, "dry_run":%t}`, len(cfg.Projects), *dryRun)
	})

	mux.HandleFunc("GET /metrics", metrics.Handler())

	mux.HandleFunc("GET /discovery/llms", func(w http.ResponseWriter, r *http.Request) {
		// Discover local LLMs on the fly.
		// In a production setup this might run periodically and cache the results.
		sources := discovery.DiscoverLocalLLMs(r.Context(), cfg)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "ok",
			"sources": sources,
		})
	})

	mux.HandleFunc("GET /projects/{name}/state", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		mu.RLock()
		machine, ok := machines[name]
		mu.RUnlock()

		if !ok {
			http.Error(w, `{"error":"project not found"}`, http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(machine.GetDiagnostics())
	})

	mux.HandleFunc("POST /projects/{name}/backup", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		mu.RLock()
		machine, ok := machines[name]
		mu.RUnlock()

		if !ok {
			http.Error(w, `{"error":"project not found"}`, http.StatusNotFound)
			return
		}

		if err := machine.Transition(ctx, state.EventTriggerBackup); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusConflict)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"backup_triggered", "project":"%s"}`, name)
	})

	mux.HandleFunc("POST /projects/{name}/reset", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		mu.RLock()
		machine, ok := machines[name]
		mu.RUnlock()

		if !ok {
			http.Error(w, `{"error":"project not found"}`, http.StatusNotFound)
			return
		}

		if err := machine.Transition(ctx, state.EventClearError); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusConflict)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"error_cleared", "project":"%s"}`, name)
	})

	if cfg.System.AgentSkills.Enabled {
		for _, method := range cfg.System.AgentSkills.ExposeVia {
			if method == "mcp" {
				mux.HandleFunc("POST /mcp", skillAggregator.HandleMCP)
				slog.Info("MCP Server exposed on POST /mcp")
				break
			}
		}
	}

	port := cfg.System.Daemon.ControlPort
	if port == 0 {
		port = 8080
	}

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	go func() {
		slog.Info("starting control port", "port", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("control port failure", "error", err)
			cancel()
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigChan:
		slog.Info("received termination signal", "signal", sig)
	case <-ctx.Done():
		slog.Info("root context cancelled")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("control port forced to shutdown abruptly", "error", err)
	}

	slog.Info("shutdown complete")
}
