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
	if flagValue != "" {
		return flagValue
	}

	if envPath := os.Getenv("DOCKER_PLUGIN_SOCKET"); envPath != "" {
		log.WithField("source", "DOCKER_PLUGIN_SOCKET").Info("Socket path from environment variable")
		return envPath
	}

	// Auto-discover: Docker mounts /run/docker/plugins/<PLUGIN_ID>/ inside the plugin container.
	pluginsDir := "/run/docker/plugins"
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
				socketPath := filepath.Join(pluginsDir, pluginDirs[0], "net-dhcp.sock")
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

	defaultPath := "/run/docker/plugins/net-dhcp.sock"
	log.WithField("fallback", defaultPath).Info("Using fallback socket path")
	return defaultPath
}

// waitForDockerReady blocks until dockerd is ready, retrying every 500ms up to 30 seconds.
func waitForDockerReady(ctx context.Context, client *docker.Client) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("docker daemon not ready after 30 seconds")
		}
		if _, err := client.Ping(ctx, docker.PingOptions{}); err == nil {
			log.Debug("Docker daemon is ready")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func main() {
	flag.Parse()

	actualSocketPath := discoverSocketPath(*bindSock)

	// Log level: flag > DOCKER_NETWORK_DHCP_LOG_LEVEL env var > default "info"
	if *logLevel == "" {
		if *logLevel = os.Getenv("DOCKER_NETWORK_DHCP_LOG_LEVEL"); *logLevel == "" {
			*logLevel = "info"
		}
	}

	level, err := log.ParseLevel(*logLevel)
	if err != nil {
		log.WithError(err).Fatal("Failed to parse log level")
	}
	log.SetLevel(level)

	// Log file: flag > DOCKER_NETWORK_DHCP_LOGFILE env var > stderr
	if *logFile == "" {
		*logFile = os.Getenv("DOCKER_NETWORK_DHCP_LOGFILE")
	}
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			log.WithError(err).Fatal("Failed to open log file for writing")
		}
		defer f.Close()
		log.StandardLogger().Out = f
	}

	log.WithFields(log.Fields{
		"log_level": *logLevel,
		"socket":    actualSocketPath,
		"logfile":   *logFile,
	}).Info("Plugin starting up")

	// Await timeout: DOCKER_NETWORK_DHCP_AWAIT_TIMEOUT env var > default 10s
	awaitTimeout := 10 * time.Second
	if t, ok := os.LookupEnv("DOCKER_NETWORK_DHCP_AWAIT_TIMEOUT"); ok {
		awaitTimeout, err = time.ParseDuration(t)
		if err != nil {
			log.WithError(err).Fatal("Failed to parse await timeout")
		}
	}
	log.WithField("await_timeout", awaitTimeout).Debug("Container await timeout configured")

	p, err := plugin.NewPlugin(awaitTimeout)
	if err != nil {
		log.WithError(err).Fatal("Failed to create plugin")
	}

	// Re-attach persistent DHCP clients to containers that survived a daemon restart.
	// Runs in the background with a 10s delay (plugin starts before dockerd finishes
	// "Loading containers"). The 5-minute timeout is documented in Plugin.StartRestore().
	p.StartRestore()

	// Wait for Docker daemon before opening the plugin socket to avoid races.
	if err := waitForDockerReady(context.Background(), p.Client()); err != nil {
		log.WithError(err).Fatal("Docker daemon did not become ready in time")
	}
	log.Info("Docker daemon is ready — plugin can now handle requests")

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.WithFields(log.Fields{
					"panic":     fmt.Sprintf("%v", r),
					"goroutine": "http-server",
				}).Error("CRITICAL: HTTP server goroutine panicked")
			}
		}()
		log.Info("Starting server...")
		if err := p.Listen(actualSocketPath); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.WithError(err).Fatal("Failed to start plugin")
		}
		log.Info("HTTP server stopped")
	}()

	// Verify socket creation after a short delay.
	go func() {
		time.Sleep(500 * time.Millisecond)
		if info, err := os.Stat(actualSocketPath); err == nil {
			log.WithFields(log.Fields{
				"socket": actualSocketPath,
				"mode":   fmt.Sprintf("%#o", info.Mode()),
			}).Info("Plugin socket created and accessible")
		} else {
			log.WithError(err).Warn("Socket verification failed — plugin may not be reachable")
		}
	}()

	<-sigs
	log.Info("Shutting down...")
	if err := p.Close(); err != nil {
		log.WithError(err).Fatal("Failed to stop plugin")
	}
}
