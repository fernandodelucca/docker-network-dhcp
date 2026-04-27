package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
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
	logLevel = flag.String("log", "", "log level")
	logFile  = flag.String("logfile", "", "log file")
	bindSock = flag.String("sock", "", "bind unix socket (auto-discovered if empty)")
)

// discoverSocketPath finds the Docker plugin socket directory mounted by Docker daemon.
// Docker mounts /run/docker/plugins/<PLUGIN_ID>/ inside the plugin container.
// This function attempts to discover that directory automatically.
func discoverSocketPath(flagValue string) string {
	// 1. If explicitly set via flag/env, use that
	if flagValue != "" {
		return flagValue
	}

	// 2. Try environment variable (allows manual override)
	if envPath := os.Getenv("DOCKER_PLUGIN_SOCKET"); envPath != "" {
		log.WithField("source", "DOCKER_PLUGIN_SOCKET").Info("Socket path from environment variable")
		return envPath
	}

	// 3. Try to auto-discover: Docker mounts /run/docker/plugins/<PLUGIN_ID>/
	// If running as a Docker plugin, there should be exactly one mounted directory there
	pluginsDir := "/run/docker/plugins"
	if info, err := os.Stat(pluginsDir); err == nil && info.IsDir() {
		entries, err := ioutil.ReadDir(pluginsDir)
		if err == nil {
			// Count subdirectories (plugin IDs)
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
				}).Warn("Multiple plugin directories found, cannot auto-discover — will use fallback")
			}
		}
	}

	// 4. Fallback: use default path (for development/testing)
	defaultPath := "/run/docker/plugins/net-dhcp.sock"
	log.WithField("fallback", defaultPath).Info("Using fallback socket path")
	return defaultPath
}

// waitForDockerReady blocks until dockerd is ready to accept API calls, with 500ms retry intervals up to 30 seconds
func waitForDockerReady(ctx context.Context, client *docker.Client) error {
	deadline := time.Now().Add(30 * time.Second)
	interval := 500 * time.Millisecond
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("docker daemon not ready after 30 seconds")
		}
		_, err := client.Ping(ctx, docker.PingOptions{})
		if err == nil {
			log.Debug("Docker daemon is ready")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func main() {
	flag.Parse()

	// Discover socket path: explicit flag > environment variable > auto-discovery > fallback
	actualSocketPath := discoverSocketPath(*bindSock)

	if *logLevel == "" {
		if *logLevel = os.Getenv("LOG_LEVEL"); *logLevel == "" {
			*logLevel = "info"
		}
	}

	level, err := log.ParseLevel(*logLevel)
	if err != nil {
		log.WithError(err).Fatal("Failed to parse log level")
	}
	log.SetLevel(level)

	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
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

	awaitTimeout := 10 * time.Second
	if t, ok := os.LookupEnv("AWAIT_TIMEOUT"); ok {
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

	// Re-attach persistent DHCP clients to containers that survived a daemon
	// restart. Runs in the background because the plugin starts before dockerd
	// finishes "Loading containers" — Restore handles its own readiness wait.
	// The 10s delay and 5-minute timeout are documented in Plugin.StartRestore().
	p.StartRestore()

	// Wait for Docker daemon to be ready before listening for connections.
	// This ensures dockerd won't see the plugin socket before it can handle requests.
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
		// http.ErrServerClosed is the expected return when Close() is called
		// during graceful shutdown — don't escalate that to Fatal.
		if err := p.Listen(actualSocketPath); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.WithError(err).Fatal("Failed to start plugin")
		}
		log.Info("HTTP server stopped")
	}()

	// Verify socket is created and accessible
	go func() {
		time.Sleep(500 * time.Millisecond)
		if info, err := os.Stat(actualSocketPath); err == nil {
			log.WithFields(log.Fields{
				"socket":    actualSocketPath,
				"mode":      fmt.Sprintf("%#o", info.Mode()),
				"size":      info.Size(),
			}).Info("Plugin socket created and accessible")
		} else {
			log.WithError(err).Warn("Socket verification failed")
		}
	}()

	<-sigs
	log.Info("Shutting down...")
	if err := p.Close(); err != nil {
		log.WithError(err).Fatal("Failed to stop plugin")
	}
}
