package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	docker "github.com/moby/moby/client"
	log "github.com/sirupsen/logrus"

	"github.com/fernandodelucca/docker-network-dhcp/pkg/plugin"
)

var (
	logLevel = flag.String("log", "", "log level")
	logFile  = flag.String("logfile", "", "log file")
	bindSock = flag.String("sock", "/run/docker/plugins/net-dhcp.sock", "bind unix socket")
)

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
		"socket":    *bindSock,
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
		if err := p.Listen(*bindSock); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.WithError(err).Fatal("Failed to start plugin")
		}
		log.Info("HTTP server stopped")
	}()

	// Verify socket is created and accessible
	go func() {
		time.Sleep(500 * time.Millisecond)
		if info, err := os.Stat(*bindSock); err == nil {
			log.WithFields(log.Fields{
				"socket":    *bindSock,
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
