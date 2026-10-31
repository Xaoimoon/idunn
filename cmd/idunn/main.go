// Command idunn keeps Docker Swarm services, or the standalone containers
// of one engine, on the latest digest of their image tag.
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

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/robfig/cron/v3"

	"github.com/xaoimoon/idunn/internal/cleanup"
	"github.com/xaoimoon/idunn/internal/cluster"
	"github.com/xaoimoon/idunn/internal/config"
	"github.com/xaoimoon/idunn/internal/engine"
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
		// Before anything else, including reading secrets. In engine mode
		// Iðunn keeps running as root, like Watchtower.
		if cmd != "cleanup" && engineMode() {
			break
		}
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

// infoer is the part of the Docker API that describes the engine.
type infoer interface {
	Info(ctx context.Context, options client.InfoOptions) (client.SystemInfoResult, error)
}

// resolveMode turns IDUNN_MODE into swarm or engine: auto picks engine only
// when the engine is not part of a Swarm at all. Any other Swarm state
// (pending, locked, error) stays in Swarm mode, which then reports it.
func resolveMode(ctx context.Context, docker infoer, mode string) (string, error) {
	switch mode {
	case config.ModeSwarm, config.ModeEngine:
		return mode, nil
	}
	res, err := docker.Info(ctx, client.InfoOptions{})
	if err != nil {
		return "", err
	}
	switch res.Info.Swarm.LocalNodeState {
	case swarm.LocalNodeStateInactive, "":
		return config.ModeEngine, nil
	}
	return config.ModeSwarm, nil
}

// engineMode tells, before privileges are dropped, whether Iðunn runs in
// engine mode. When the engine cannot be reached, it answers no: the
// privileges are dropped and the error shows up later.
func engineMode() bool {
	mode := strings.ToLower(os.Getenv("IDUNN_MODE"))
	switch mode {
	case config.ModeSwarm, config.ModeEngine:
		return mode == config.ModeEngine
	}
	docker, err := dockerClient()
	if err != nil {
		return false
	}
	defer docker.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := resolveMode(ctx, docker, mode)
	return err == nil && m == config.ModeEngine
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

	mode, err := resolveMode(ctx, docker, cfg.Mode)
	if err != nil {
		return err
	}
	var nodeID string
	var self cluster.Self
	if mode == config.ModeSwarm {
		if nodeID, err = cluster.LocalNodeID(ctx, docker); err != nil {
			return err
		}
		self, err = cluster.FindSelf(ctx, docker)
		if err != nil {
			log.Warn("cannot identify own service; self-exclusion relies on labels and image cleanup is disabled", "error", err)
		}
	} else {
		self, err = cluster.FindSelf(ctx, docker)
		if err != nil {
			log.Warn("cannot identify own container; self-update is disabled", "error", err)
		} else if stop, err := handOverOnStart(ctx, docker, &self, log); err != nil {
			return err
		} else if stop {
			return nil
		}
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
	if mode == config.ModeEngine {
		u.Engine = docker
	}
	if cfg.MetricsAddr != "" {
		reg, stopMetrics, err := serveMetrics(cfg.MetricsAddr, log)
		if err != nil {
			return err
		}
		defer stopMetrics()
		u.Metrics = reg
	}
	if mode == config.ModeSwarm {
		job := &cleanup.Job{Docker: docker, Self: self, Auth: creds, Keep: cfg.CleanupKeep, Ignore: cfg.LabelIgnore, Log: log}
		u.Cleaner = job.Run
	} else {
		u.Cleaner = func(ctx context.Context, repos []string) []notify.Event {
			if err := cleanup.Local(ctx, docker, repos, cfg.CleanupKeep, log); err != nil {
				return []notify.Event{{Kind: notify.KindCleanupFailed, Level: notify.Warn, Time: time.Now(),
					Nodes: []string{hostName(ctx, docker)}, Message: err.Error()}}
			}
			return nil
		}
	}

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

// handOverOnStart completes, in engine mode, a self-update on the side of
// the Iðunn that starts: a new instance waits for the one it replaces to
// stop; an old instance restarted after an interrupted hand-over stops if the
// new one runs. It reports whether this instance must stop.
func handOverOnStart(ctx context.Context, docker *client.Client, self *cluster.Self, log *slog.Logger) (bool, error) {
	if self.ContainerID == "" {
		return false, nil
	}
	me, err := engine.Inspect(ctx, docker, self.ContainerID)
	if err != nil {
		return false, err
	}
	superseded, err := engine.Superseded(ctx, docker, me, log)
	if err != nil {
		log.Warn("cannot check an interrupted self-update", "error", err)
	} else if superseded {
		log.Info("a newer instance replaces this one, stopping")
		// Stopping our own container through the API keeps its restart
		// policy from starting it again; SIGTERM then ends the process.
		go docker.ContainerStop(context.WithoutCancel(ctx), me.ID, client.ContainerStopOptions{})
		<-ctx.Done()
		return true, nil
	}
	self.Name = me.Name
	if err := engine.AwaitPredecessor(ctx, docker, me, 0, log); err != nil {
		if ctx.Err() != nil {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// hostName returns the engine's host name, as shown in notifications.
func hostName(ctx context.Context, docker infoer) string {
	if res, err := docker.Info(ctx, client.InfoOptions{}); err == nil && res.Info.Name != "" {
		return res.Info.Name
	}
	h, _ := os.Hostname()
	return h
}

// startup logs the configuration and the tracked services on every
// instance; only the leader sends the startup notification, or the single
// instance in engine mode.
func startup(ctx context.Context, u *updater.Updater, log *slog.Logger, nodeID, scheduleDesc string) {
	cfg := u.Config
	host, _ := os.Hostname()
	nodeName := nodeID
	backend := config.ModeSwarm
	if u.Engine != nil {
		backend = config.ModeEngine
		nodeName = hostName(ctx, u.Docker)
	} else if nodes, err := cluster.Nodes(ctx, u.Docker); err == nil {
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

	names, err := u.TrackedNames(ctx)
	if err != nil {
		log.Error("cannot list services", "error", err)
	}
	versionCheck := "off"
	if cfg.VersionCheck {
		versionCheck = cfg.VersionSchedule + " (" + cfg.VersionLevel.String() + ")"
	}
	log.Info("idunn started",
		"version", version, "backend", backend, "node", nodeName, "container", host,
		"schedule", scheduleDesc, "version_check", versionCheck, "timezone", time.Local.String(),
		"blackout", strings.Join(blackout, ","), "filter", cfg.LabelFilter.String(),
		"ignore", cfg.LabelIgnore.String(), "min_age", cfg.MinAge.String(),
		"mode", strings.Join(mode, ","), "self_update", cfg.SelfUpdate, "metrics", cfg.MetricsAddr, "tracked", len(names))
	log.Info("tracked services", "services", strings.Join(names, " "))

	leader := u.Engine != nil
	if !leader {
		ok, err := cluster.IsLeader(ctx, u.Docker, nodeID)
		leader = err == nil && ok
	}
	if leader {
		u.Notifier.Send(ctx, []notify.Event{{
			Kind: notify.KindStartup, Level: notify.Info, Time: time.Now(),
			Version: version, Node: nodeName, Schedule: scheduleDesc,
			Blackout: strings.Join(blackout, ","), Tracked: len(names), Mode: strings.Join(mode, ", "),
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
