// Command idunn keeps Docker Swarm services on the latest digest of their
// image tag.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // the image has no zoneinfo; TZ must still work

	"github.com/moby/moby/client"
	"github.com/robfig/cron/v3"

	"github.com/xaoimoon/idunn/internal/cleanup"
	"github.com/xaoimoon/idunn/internal/cluster"
	"github.com/xaoimoon/idunn/internal/config"
	"github.com/xaoimoon/idunn/internal/metrics"
	"github.com/xaoimoon/idunn/internal/notify"
	"github.com/xaoimoon/idunn/internal/privdrop"
	"github.com/xaoimoon/idunn/internal/registry"
	"github.com/xaoimoon/idunn/internal/schedule"
	"github.com/xaoimoon/idunn/internal/updater"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "", "run", "health", "cleanup":
		// Before anything else, including reading secrets.
		if who, dropErr := privdrop.Drop(socketPath(), unprivilegedUID); dropErr != nil {
			fmt.Fprintln(os.Stderr, "idunn: cannot drop root privileges:", dropErr)
			os.Exit(1)
		} else if who != "" && cmd != "health" {
			fmt.Fprintln(os.Stderr, "idunn: dropped root privileges,", who)
		}
	}
	switch cmd {
	case "", "run":
		err = run()
	case "health":
		err = health()
	case "cleanup":
		err = runCleanup(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		fmt.Println("usage: idunn [run|health|cleanup|version]\nConfiguration is read from IDUNN_* environment variables.")
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "idunn:", err)
		os.Exit(1)
	}
}

func newLogger(level, format string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	opts := &slog.HandlerOptions{Level: l}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

// unprivilegedUID is the user Iðunn switches to when started as root
// (the "nonroot" user of distroless images).
const unprivilegedUID = 65532

// socketPath returns the Docker socket Iðunn talks to, or "" for a TCP host.
func socketPath() string {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		return "/var/run/docker.sock"
	}
	if p, ok := strings.CutPrefix(host, "unix://"); ok {
		return p
	}
	return ""
}

func dockerClient() (*client.Client, error) {
	return client.New(client.FromEnv, client.WithUserAgent("idunn/"+version))
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel, cfg.LogFormat)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	docker, err := dockerClient()
	if err != nil {
		return err
	}
	defer docker.Close()

	nodeID, err := cluster.LocalNodeID(ctx, docker)
	if err != nil {
		return err
	}
	self, err := cluster.FindSelf(ctx, docker)
	if err != nil {
		log.Warn("cannot identify own service; self-exclusion relies on labels and image cleanup is disabled", "error", err)
	}

	creds, err := registry.LoadCredentials(cfg.RegistryAuthFile)
	if err != nil {
		return err
	}
	checker, err := registry.NewChecker(creds, "idunn/"+version, false)
	if err != nil {
		return err
	}
	notifier, err := notify.New(notify.Options{
		URLs:     cfg.NotificationURLs,
		Lang:     cfg.NotificationLang,
		Template: cfg.NotificationTemplate,
		Level:    notify.ParseLevel(cfg.NotificationLevel),
		Report:   cfg.NotificationReport,
	}, log)
	if err != nil {
		return err
	}

	u := &updater.Updater{
		Docker:   docker,
		Checker:  checker,
		Lister:   checker,
		Auth:     creds,
		Notifier: notifier,
		Config:   cfg,
		Log:      log,
		NodeID:   nodeID,
		Self:     self,
	}
	if cfg.MetricsAddr != "" {
		reg, stopMetrics, err := serveMetrics(cfg.MetricsAddr, log)
		if err != nil {
			return err
		}
		defer stopMetrics()
		u.Metrics = reg
	}
	job := &cleanup.Job{Docker: docker, Self: self, Auth: creds, Keep: cfg.CleanupKeep, Ignore: cfg.LabelIgnore, Log: log}
	u.Cleaner = job.Run

	scheduleDesc := cfg.Schedule
	if cfg.Interval > 0 {
		scheduleDesc = "every " + cfg.Interval.String()
	}
	startup(ctx, u, log, nodeID, scheduleDesc)

	if cfg.RunOnce {
		u.Pass(ctx)
		if cfg.VersionCheck {
			u.VersionCheck(ctx)
		}
		return nil
	}

	// The version check has its own daily schedule, whatever drives the passes.
	if cfg.VersionCheck {
		vc := cron.New(cron.WithSeconds(), cron.WithLocation(time.Local))
		if _, err := vc.AddFunc(cfg.VersionSchedule, func() { u.VersionCheck(ctx) }); err != nil {
			return fmt.Errorf("IDUNN_VERSION_SCHEDULE: %w", err)
		}
		vc.Start()
		defer func() { <-vc.Stop().Done() }()
	}

	if cfg.Interval > 0 {
		t := time.NewTicker(cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Info("stopping")
				return nil
			case <-t.C:
				u.Pass(ctx)
			}
		}
	}

	c := cron.New(cron.WithSeconds(), cron.WithLocation(time.Local))
	if _, err := c.AddFunc(cfg.Schedule, func() { u.Pass(ctx) }); err != nil {
		return fmt.Errorf("IDUNN_SCHEDULE: %w", err)
	}
	c.Start()
	<-ctx.Done()
	log.Info("stopping, waiting for the current pass")
	<-c.Stop().Done()
	return nil
}

// startup logs the configuration and the tracked services on every
// instance; only the leader sends the startup notification.
func startup(ctx context.Context, u *updater.Updater, log *slog.Logger, nodeID, scheduleDesc string) {
	cfg := u.Config
	host, _ := os.Hostname()
	nodeName := nodeID
	if nodes, err := cluster.Nodes(ctx, u.Docker); err == nil {
		for _, n := range nodes {
			if n.ID == nodeID {
				nodeName = n.Name
			}
		}
	}
	var mode []string
	if cfg.MonitorOnly {
		mode = append(mode, "monitor-only")
	}
	if cfg.DryRun {
		mode = append(mode, "dry-run")
	}
	blackout := make([]string, len(cfg.Blackout))
	for i, w := range cfg.Blackout {
		blackout[i] = w.String()
	}

	services, err := u.Tracked(ctx)
	if err != nil {
		log.Error("cannot list services", "error", err)
	}
	names := make([]string, len(services))
	for i, s := range services {
		names[i] = s.Spec.Name
	}
	versionCheck := "off"
	if cfg.VersionCheck {
		versionCheck = cfg.VersionSchedule + " (" + cfg.VersionLevel.String() + ")"
	}
	log.Info("idunn started",
		"version", version, "node", nodeName, "container", host,
		"schedule", scheduleDesc, "version_check", versionCheck, "timezone", time.Local.String(),
		"blackout", strings.Join(blackout, ","), "filter", cfg.LabelFilter.String(),
		"ignore", cfg.LabelIgnore.String(), "min_age", cfg.MinAge.String(),
		"mode", strings.Join(mode, ","), "self_update", cfg.SelfUpdate, "metrics", cfg.MetricsAddr, "tracked", len(services))
	log.Info("tracked services", "services", strings.Join(names, " "))

	if leader, err := cluster.IsLeader(ctx, u.Docker, nodeID); err == nil && leader {
		u.Notifier.Send(ctx, []notify.Event{{
			Kind: notify.KindStartup, Level: notify.Info, Time: time.Now(),
			Version: version, Node: nodeName, Schedule: scheduleDesc,
			Blackout: strings.Join(blackout, ","), Tracked: len(services), Mode: strings.Join(mode, ", "),
		}})
	}
	if _, in := schedule.InBlackout(cfg.Blackout, time.Now()); in {
		log.Info("currently inside a blackout window")
	}
}

// serveMetrics exposes the Prometheus metrics on addr (/metrics only) and
// returns a function that stops the server.
func serveMetrics(addr string, log *slog.Logger) (*metrics.Registry, func(), error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("IDUNN_METRICS_ADDR: %w", err)
	}
	reg := metrics.New()
	reg.Set("idunn_build_info", "Iðunn version, always 1.", metrics.Labels{"version": version}, 1)
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", reg.Handler())
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("metrics server stopped", "error", err)
		}
	}()
	log.Info("metrics listening", "addr", ln.Addr().String())
	return reg, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, nil
}

// health checks that the Docker API answers; the healthcheck runs inside the
// container, so the process itself is alive by definition.
func health() error {
	docker, err := dockerClient()
	if err != nil {
		return err
	}
	defer docker.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = docker.Ping(ctx, client.PingOptions{})
	return err
}

func runCleanup(args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	keep := fs.Int("keep", 1, "unused images kept per repository")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("cleanup: no repository given")
	}
	log := newLogger(os.Getenv("IDUNN_LOG_LEVEL"), os.Getenv("IDUNN_LOG_FORMAT"))
	docker, err := dockerClient()
	if err != nil {
		return err
	}
	defer docker.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return cleanup.Local(ctx, docker, fs.Args(), *keep, log)
}
