package recovery

import (
	"runtime/debug"

	"github.com/vanpelt/catnip/internal/logger"
)

// SafeGo runs a function in a goroutine with automatic panic recovery
// This prevents any single goroutine panic from crashing the entire server
func SafeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("🚨 PANIC recovered in goroutine '%s': %v", name, r)
				logger.Errorf("Stack trace:\n%s", debug.Stack())
			}
		}()
		fn()
	}()
}

// SafeGoWithCleanup runs a function in a goroutine with panic recovery and cleanup
func SafeGoWithCleanup(name string, fn func(), cleanup func()) {
	go func() {
		defer func() {
			if cleanup != nil {
				cleanup()
			}
			if r := recover(); r != nil {
				logger.Errorf("🚨 PANIC recovered in goroutine '%s': %v", name, r)
				logger.Errorf("Stack trace:\n%s", debug.Stack())
			}
		}()
		fn()
	}()
}

// SafeGoContext runs a function with a context-like pattern for WebSocket handlers
func SafeGoContext(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("🚨 PANIC recovered in %s: %v", name, r)
				// Don't print full stack trace for WebSocket handlers to reduce noise
				if logger.Logger.GetLevel() <= 0 {
					logger.Debugf("Stack trace available in debug mode")
				}
			}
		}()
		fn()
	}()
}
