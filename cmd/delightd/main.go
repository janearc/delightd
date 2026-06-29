package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/janearc/big-little-mesh/emit"
	"github.com/janearc/big-little-mesh/frood"
	observabilityproto "github.com/janearc/big-little-mesh/proto/observability/v1"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"delightd/config"
	delightv1 "delightd/gen/go/delight/v1"
	"delightd/pkg/backup"
	"delightd/pkg/discovery"
	"delightd/pkg/events"
	"delightd/pkg/exports"
	"delightd/pkg/httpapi"
	"delightd/pkg/metrics"
	"delightd/pkg/registry"
	"delightd/pkg/skills"
	"delightd/pkg/state"
	"delightd/pkg/traefik"
	"delightd/pkg/watcher"
	delightproto "delightd/proto"
)

// publishBackupEvent emits a delight.v1.BackupEvent. It is best-effort: a nil
// publisher (disabled or unreachable Kafka) is a no-op, and any error is logged,
// never propagated -- event emission must not affect the backup it describes.
func publishBackupEvent(ctx context.Context, pub *events.Publisher, project string, success bool, res backup.CheckpointResult) {
	if pub == nil {
		return
	}
	ev := &delightv1.BackupEvent{
		ProjectName:          project,
		Success:              success,
		BytesBefore:          res.BytesBefore,
		BytesAfter:           res.BytesAfter,
		DurationMilliseconds: uint32(res.Duration.Milliseconds()),
		Timestamp:            timestamppb.Now(),
	}
	if err := pub.PublishBackup(ctx, ev); err != nil {
		slog.Warn("failed to publish backup event", "project", project, "error", err)
	}
}

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// rootCmd is the delightd command tree. The daemon is the default action; cobra
// keeps subcommand and flag parsing declarative instead of hand-rolled. `lint` is
// the first subcommand (`register`/`unregister` are the Phase 3 follow-up to #19).
func rootCmd() *cobra.Command {
	var dryRun, immediate bool
	cmd := &cobra.Command{
		Use:          "delightd",
		Short:        "delightd -- the fleet control-plane daemon",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runDaemon(dryRun, immediate)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "execute without writing checkpoints to disk")
	cmd.Flags().BoolVar(&immediate, "immediate", false, "execute an immediate evaluation on startup without waiting for the first interval tick")
	cmd.AddCommand(lintCmd())
	cmd.AddCommand(modelCmd())
	return cmd
}

// runDaemon is the long-running control plane: load config, build the per-project
// state machines, start the sync/discovery/backup loops and the control port, and
// block until a termination signal.
func runDaemon(dryRun, immediate bool) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if dryRun {
		slog.Warn("DAEMON STARTED IN DRY RUN MODE - NO DESTRUCTIVE DISK WRITES WILL OCCUR")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.Load(ctx)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// machines is built once here and only read afterwards (control loop and
	// HTTP handlers), so it needs no lock; each Machine guards its own state.
	machines := make(map[string]*state.Machine)
	for _, proj := range cfg.Projects {
		machines[proj.Name] = state.NewMachine(proj.Name)
	}

	slog.Info("configuration loaded successfully", "projects_count", len(cfg.Projects))

	// MonitorRoot is the tree delightd watches (the parent of the managed
	// projects). The export engine and skill aggregator scan it. It is
	// configurable via system.monitor_root / DELIGHT_MONITOR_ROOT and defaults
	// to ~/work; config.Load has already applied the default and expanded ~.
	monitorRoot := cfg.System.MonitorRoot
	exportEngine := exports.NewEngine(monitorRoot)
	var knownProjects []string
	for _, proj := range cfg.Projects {
		knownProjects = append(knownProjects, proj.Name)
	}

	if err := exportEngine.Sync(ctx, knownProjects, dryRun); err != nil {
		slog.Error("initial export sync failed", "error", err)
	}

	skillAggregator := skills.NewAggregator(monitorRoot)

	syncSkills := func() {
		if !cfg.System.AgentSkills.Enabled {
			return
		}
		if err := skillAggregator.ScanProjects(knownProjects); err != nil {
			slog.Error("failed to scan skills", "error", err)
		}

		// Handle CLI Generation
		for _, method := range cfg.System.AgentSkills.ExposeVia {
			if method == "cli" && !dryRun {
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
				exportEngine.Sync(ctx, knownProjects, dryRun)
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

	// Event emission is best-effort: a Kafka/SR outage must never block backups,
	// so a failed init (or no brokers configured) leaves a nil, no-op publisher.
	var publisher *events.Publisher
	if len(cfg.System.Kafka.Brokers) > 0 {
		p, err := events.New(ctx, cfg.System.Kafka.Brokers, cfg.System.Kafka.SchemaRegistryURL, cfg.System.Kafka.Topic, delightproto.BackupEventSchema)
		if err != nil {
			slog.Warn("event publishing disabled: could not init kafka publisher", "error", err)
		} else {
			publisher = p
			defer publisher.Close()
			slog.Info("kafka event publisher ready", "brokers", cfg.System.Kafka.Brokers, "topic", cfg.System.Kafka.Topic)
		}
	}

	// delightd joins the fleet as a frood: a liveness heartbeat on
	// observability.events via Big Little Mesh's frood helper, so the daemon is
	// visible to obs-svc like every other service. It rides the same broker and
	// schema-registry config as the backup publisher above, but as its own Big Little Mesh
	// emit.Publisher on the observability.v1 contracts -- the backup publisher
	// stays on delight.v1 (different contract, different topic). Best-effort and
	// nil-safe, exactly like the backup publisher: a Kafka/SR outage disables the
	// heartbeat, never the daemon's real work.
	if len(cfg.System.Kafka.Brokers) > 0 {
		emitPub, err := emit.New(ctx, cfg.System.Kafka.Brokers, cfg.System.Kafka.SchemaRegistryURL)
		if err != nil {
			slog.Warn("frood heartbeat disabled: could not init emit publisher", "error", err)
		} else {
			defer emitPub.Close()
			// 15s liveness cadence (Big Little Mesh defaults to the same when interval <= 0).
			// Heartbeat blocks, so it runs in its own goroutine until ctx cancels.
			go frood.Heartbeat(ctx, emitPub, "delightd", observabilityproto.Schema, 15*time.Second, logger)
			slog.Info("frood heartbeat started", "service", "delightd", "topic", frood.TopicObservability)
		}
	}

	for _, proj := range cfg.Projects {
		go func(p config.ProjectConfig) {
			if immediate {
				slog.Info("executing immediate startup evaluation", "project", p.Name)
				machine := machines[p.Name]

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
					machine := machines[p.Name]

					// only poll the git oracle while idle or monitoring; while a
					// backup is running or in error backoff there is nothing to react to.
					if !machine.WatchesForChurn() {
						continue
					}

					metrics.Inc(fmt.Sprintf(`delightd_git_churn_checks_total{project="%s"}`, p.Name))
					churn, err := watcher.HasChurn(ctx, p.Path)
					if err != nil {
						slog.Error("failed to poll git oracle", "project", p.Name, "error", err)
						continue
					}

					// the machine decides the right transition for its state.
					if churn {
						if err := machine.AdvanceOnChurn(ctx); err != nil {
							slog.Error("failed to advance state on churn", "project", p.Name, "error", err)
						}
					}

				case <-evalTicker.C:
					machine := machines[p.Name]

					if machine.GetState() == state.StateBackingUp {
						slog.Info("executing backup pipeline", "project", p.Name)

						// BackupsRoot is the backup destination directory itself
						// (delight.yaml default ~/var/backups, derived as
						// ${DaemonRoot}/backups; DELIGHT_BACKUPS_ROOT=/var/backups in
						// compose+kube, both resolving to ~/var/backups on the host).
						// CreateCheckpoint writes archives here directly -- no "/backups"
						// is appended, which previously doubled the segment.
						res, err := backup.CreateCheckpoint(ctx, p.Name, p.Path, cfg.System.BackupsRoot, p.Backup.Rotation.MaxArchives, p.Backup.Exclude, dryRun)
						if err != nil {
							metrics.Inc(fmt.Sprintf(`delightd_backup_failures_total{project="%s"}`, p.Name))
							slog.Error("backup pipeline failed", "project", p.Name, "error", err)
							publishBackupEvent(ctx, publisher, p.Name, false, backup.CheckpointResult{})
							machine.Transition(ctx, state.EventBackupFail)
						} else {
							metrics.Inc(fmt.Sprintf(`delightd_backup_success_total{project="%s"}`, p.Name))
							slog.Info("backup pipeline succeeded", "project", p.Name, "archive", res.ArchivePath)
							publishBackupEvent(ctx, publisher, p.Name, true, res)
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

	// The live frood registry (the /register broker's state). Its snapshot is a
	// warm-start cache under DaemonRoot; loading it on boot means discovery is available
	// immediately rather than blank until the first re-register. Additive: it sits
	// alongside the yaml/poll roster and replaces nothing. A failed load is logged and the
	// daemon continues with an empty registry (the availability mandate: come up anyway).
	reg := registry.New(filepath.Join(cfg.System.DaemonRoot, "registry", "registrations.json"), slog.Default())
	if err := reg.Load(); err != nil {
		slog.Error("registry: warm-start load failed; continuing with empty registry", "error", err)
	}

	// The control-port HTTP surface lives in pkg/httpapi so handlers are
	// unit-testable; main retains only wiring and the daemon control loop.
	api := httpapi.New(cfg, machines, exportEngine, skillAggregator, dryRun, reg)
	mux := api.Mux()

	// Resolve to the canonical control port (config.DefaultControlPort = 8088) when
	// the config leaves it unset, so the listener always lands where compose, kube,
	// and every client route.
	port := cfg.System.Daemon.ResolveControlPort()

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
	return nil
}
