package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	docker "github.com/moby/moby/client"
	log "github.com/sirupsen/logrus"

	"github.com/fernandodelucca/docker-network-dhcp/pkg/plugin"
)

var (
	logLevel = flag.String("log", "", "log level (overrides DOCKER_NETWORK_DHCP_LOG_LEVEL)")
	logFile  = flag.String("logfile", "", "log file (overrides DOCKER_NETWORK_DHCP_LOGFILE)")
	bindSock = flag.String("sock", "", "bind unix socket (auto-discovered if empty)")
)

// discoverSocketPath finds the Docker plugin socket directory mounted by Docker daemon.
// Docker mounts /run/docker/plugins/<PLUGIN_ID>/ inside the plugin container.
// Priority: explicit flag > DOCKER_PLUGIN_SOCKET env var > auto-discovery > fallback.
func discoverSocketPath(flagValue string) string {
	return resolveSocketPath(flagValue, os.Getenv("DOCKER_PLUGIN_SOCKET"), "/run/docker/plugins", "net-dhcp.sock")
}

// resolveSocketPath is the testable core of discoverSocketPath. Pure inputs,
// no global state — main() supplies the real /run/docker/plugins; tests pass
// a t.TempDir().
func resolveSocketPath(flagValue, envValue, pluginsDir, socketName string) string {
	if flagValue != "" {
		return flagValue
	}

	if envValue != "" {
		log.WithField("source", "DOCKER_PLUGIN_SOCKET").Info("Socket path from environment variable")
		return envValue
	}

	if info, err := os.Stat(pluginsDir); err == nil && info.IsDir() {
		entries, err := os.ReadDir(pluginsDir)
		if err == nil {
			var pluginDirs []string
			for _, entry := range entries {
				if entry.IsDir() {
					pluginDirs = append(pluginDirs, entry.Name())
				}
			}
			if len(pluginDirs) == 1 {
				socketPath := filepath.Join(pluginsDir, pluginDirs[0], socketName)
				log.WithField("auto_discovered", socketPath).Info("Auto-discovered socket path")
				return socketPath
			} else if len(pluginDirs) > 1 {
				log.WithFields(log.Fields{
					"plugin_dirs": pluginDirs,
					"count":       len(pluginDirs),
				}).Warn("Multiple plugin directories found, cannot auto-discover — using fallback")
			}
		}
	}

	defaultPath := filepath.Join(pluginsDir, socketName)
	log.WithField("fallback", defaultPath).Info("Using fallback socket path")
	return defaultPath
}

// monitorDockerReady polls the Docker daemon in the background and logs when it
// becomes responsive. CRITICAL: this function NEVER terminates the plugin and
// NEVER blocks startup. Operations that need the Docker API have their own
// retry/fallback logic. The plugin must remain alive and listening on its
// socket regardless of Docker daemon state — otherwise dockerd marks the plugin
// as failed/disabled, which is exactly the production issue we are fixing.
func monitorDockerReady(ctx context.Context, client *docker.Client) {
	const interval = 500 * time.Millisecond
	start := time.Now()
	attempts := 0
	logged := false
	for {
		attempts++
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := client.Ping(pingCtx, docker.PingOptions{})
		cancel()
		if err == nil {
			log.WithFields(log.Fields{
				"attempts": attempts,
				"elapsed":  time.Since(start).Round(100 * time.Millisecond),
			}).Info("Docker daemon is responsive")

			// Pull server info now that the daemon is up. Best-effort.
			if info, err := client.ServerVersion(ctx, docker.ServerVersionOptions{}); err == nil {
				log.WithFields(log.Fields{
					"api_version":    info.APIVersion,
					"os":             info.Os,
					"arch":           info.Arch,
					"docker_version": info.Version,
				}).Info("Connected to Docker daemon")
			}
			return
		}

		// First failure: warn once. Subsequent failures: trace only (avoid log spam).
		if !logged {
			log.WithError(err).Warn("Docker daemon not responsive yet — will keep retrying in background, plugin remains operational")
			logged = true
		} else {
			log.WithError(err).WithField("attempt", attempts).Trace("Docker daemon still not responsive")
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func main() {
	flag.Parse()

	actualSocketPath := discoverSocketPath(*bindSock)

	// Log level: flag > env var > default "info". Invalid input → fall back, never exit.
	if *logLevel == "" {
		if *logLevel = os.Getenv("DOCKER_NETWORK_DHCP_LOG_LEVEL"); *logLevel == "" {
			*logLevel = "info"
		}
	}
	level, err := log.ParseLevel(*logLevel)
	if err != nil {
		log.WithError(err).WithField("input", *logLevel).Warn("Invalid log level — defaulting to info")
		level = log.InfoLevel
	}
	log.SetLevel(level)

	// Log file: flag > env var > stderr. Failure to open the file falls back to
	// stderr instead of exiting — a log destination problem must never bring
	// down the plugin and cause it to be marked as failed by dockerd.
	if *logFile == "" {
		*logFile = os.Getenv("DOCKER_NETWORK_DHCP_LOGFILE")
	}
	if *logFile != "" {
		if f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600); err == nil {
			defer f.Close()
			log.StandardLogger().Out = f
		} else {
			log.WithError(err).WithField("path", *logFile).Warn("Failed to open log file — using stderr")
		}
	}

	log.WithFields(log.Fields{
		"log_level": level,
		"socket":    actualSocketPath,
		"logfile":   *logFile,
	}).Info("Plugin starting up")

	// Await timeout. Invalid value → use default, do not exit.
	awaitTimeout := 10 * time.Second
	if t, ok := os.LookupEnv("DOCKER_NETWORK_DHCP_AWAIT_TIMEOUT"); ok {
		if d, err := time.ParseDuration(t); err == nil {
			awaitTimeout = d
		} else {
			log.WithError(err).WithField("input", t).Warn("Invalid await timeout — using default 10s")
		}
	}

	p, err := plugin.NewPlugin(awaitTimeout)
	if err != nil {
		// NewPlugin only fails on a programming/config error (cannot construct
		// a Docker client object). This is genuinely fatal — without a client,
		// nothing works. NewPlugin does NOT call the Docker API.
		log.WithError(err).Fatal("Failed to construct plugin")
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	// CRITICAL ORDERING: start the HTTP server BEFORE any blocking work.
	// dockerd watches for the plugin socket to appear and times out (~30s)
	// if it doesn't see it. If we block on Docker readiness or anything else
	// before listening, dockerd will mark the plugin as failed/disabled —
	// after a reboot the plugin comes back DISABLED and every container that
	// uses it fails to start. This is the exact production issue this fix
	// addresses.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.WithFields(log.Fields{
					"panic":     fmt.Sprintf("%v", r),
					"goroutine": "http-server",
				}).Error("CRITICAL: HTTP server goroutine panicked")
			}
		}()
		log.WithField("socket", actualSocketPath).Info("Starting HTTP server on plugin socket")
		if err := p.Listen(actualSocketPath); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.WithError(err).Error("HTTP server stopped with error — initiating shutdown")
			// Initiating SIGTERM lets the main goroutine clean up gracefully.
			// Returning a non-zero exit also tells dockerd this run failed,
			// but only after a real fatal error (port bind failure, etc.).
			select {
			case sigs <- syscall.SIGTERM:
			default:
			}
		}
		log.Info("HTTP server stopped")
	}()

	// Verify the socket appeared. Non-fatal — dockerd will tell us soon enough
	// if it can't reach us, and the user has /healthz for monitoring.
	go func() {
		time.Sleep(500 * time.Millisecond)
		if info, err := os.Stat(actualSocketPath); err == nil {
			log.WithFields(log.Fields{
				"socket": actualSocketPath,
				"mode":   fmt.Sprintf("%#o", info.Mode()),
			}).Info("Plugin socket created and accessible")
		} else {
			log.WithError(err).WithField("socket", actualSocketPath).Warn("Socket verification failed — plugin may not be reachable")
		}
	}()

	// Background: monitor Docker daemon readiness. Strictly informational.
	// Does NOT block startup. Does NOT terminate the plugin if Docker is slow.
	// Operations that need the Docker API retry on their own.
	monitorCtx, monitorCancel := context.WithCancel(context.Background())
	go monitorDockerReady(monitorCtx, p.Client())

	// Background: re-attach DHCP clients to surviving containers (10s delay,
	// 5min total). Independent of Docker API readiness — uses SandboxKey
	// directly via netns.GetFromPath, no Docker API call needed.
	p.StartRestore()

	<-sigs
	log.Info("Shutting down...")
	monitorCancel()
	if err := p.Close(); err != nil {
		log.WithError(err).Error("Errors during shutdown")
	}
}
