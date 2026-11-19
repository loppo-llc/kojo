package server

import (
	"context"
	"time"

	"github.com/loppo-llc/kojo/internal/eventbus"
)

// Periodic extension update pass.
//
// Every extensionUpdateInterval kojo asks each installed package's
// remote whether its ref has moved (git ls-remote — names only, no
// objects) and, for packages the operator opted into auto-update,
// applies the new commit and restarts the package's service. The pass
// mirrors selfupdate's GitHub release check in cadence; what it may do
// is bounded by extpkg: a commit that changes the scope set or needs a
// newer kojo is reported, never applied.

const (
	// extensionUpdateInitialDelay keeps the first pass out of startup,
	// when extension services are still coming up.
	extensionUpdateInitialDelay = 3 * time.Minute
	extensionUpdateInterval     = 6 * time.Hour
)

// StartExtensionUpdateLoop runs the update pass until ctx ends. A no-op
// without a registry (peer-only nodes).
func (s *Server) StartExtensionUpdateLoop(ctx context.Context) {
	if s == nil || s.extensions == nil {
		return
	}
	go func() {
		t := time.NewTimer(extensionUpdateInitialDelay)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			s.runExtensionUpdatePass(ctx)
			t.Reset(extensionUpdateInterval)
		}
	}()
}

// runExtensionUpdatePass checks every package once and applies the
// auto-updates it finds.
func (s *Server) runExtensionUpdatePass(ctx context.Context) {
	updated := s.extensions.CheckAndAutoUpdate(ctx, extensionCheckTimeout, extensionGitTimeout)
	if len(updated) > 0 {
		// Reconcile restarts the services whose commit changed.
		s.publishExtensionsChanged()
		return
	}
	// Nothing on disk changed, but the UI's update badges did.
	s.PublishEvent(eventbus.Event{Table: "extensions", Op: "update"})
}
