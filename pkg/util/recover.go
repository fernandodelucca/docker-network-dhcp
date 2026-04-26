package util

import (
	"fmt"
	"runtime/debug"

	log "github.com/sirupsen/logrus"
)

// RecoverGoroutine is a deferred panic recovery handler for long-running goroutines
// It logs the panic with context and prevents the entire plugin from crashing
func RecoverGoroutine(name string, fields log.Fields) {
	if r := recover(); r != nil {
		logFields := log.Fields{
			"goroutine": name,
			"panic":     fmt.Sprintf("%v", r),
			"stack":     string(debug.Stack()),
		}
		for k, v := range fields {
			logFields[k] = v
		}
		log.WithFields(logFields).Error("CRITICAL: goroutine panicked — recovered from panic")
	}
}
