package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/fernandodelucca/docker-network-dhcp/pkg/plugin"
)

var (
	logLevel = flag.String("log", "", "log level")
	logFile  = flag.String("logfile", "", "log file")
	bindSock = flag.String("sock", "/run/docker/plugins/net-dhcp.sock", "bind unix socket")
)

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

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, unix.SIGINT, unix.SIGTERM)

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
	}()

	<-sigs
	log.Info("Shutting down...")
	if err := p.Close(); err != nil {
		log.WithError(err).Fatal("Failed to stop plugin")
	}
}
