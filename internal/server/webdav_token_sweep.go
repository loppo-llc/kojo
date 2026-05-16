package server

import (
	"context"
	"time"
)

// webdavTokenSweepInterval is how often the background goroutine asks
// the WebDAVTokenStore to drop expired rows. WebDAV tokens have a TTL
// floor of 5 min (auth.webdavMinTTL); sweeping every 5 min means a
// freshly-expired token is reachable from the cache for at most one
// sweep tick before its row is physically purged. That window is
// further bounded by Verify's own expiry re-check (it rejects expired
// rows even between sweeps), so the sweeper here is purely a kv-side
// cleanup — not a verifier integrity guarantee.
const webdavTokenSweepInterval = 5 * time.Minute

// StartWebDAVTokenSweep launches a goroutine that periodically deletes
// expired WebDAV-token rows from kv + memory. Cancelling ctx stops the
// goroutine. Safe to call multiple times — the second call is a no-op.
func (s *Server) StartWebDAVTokenSweep(ctx context.Context) {
	if s == nil || s.webdavTokens == nil {
		return
	}
	s.webdavSweepOnce.Do(func() {
		go s.webdavTokenSweepLoop(ctx)
	})
}

func (s *Server) webdavTokenSweepLoop(ctx context.Context) {
	t := time.NewTicker(webdavTokenSweepInterval)
	defer t.Stop()
	// Run once at startup so a binary that was offline long enough
	// to let dozens of tokens expire doesn't have to wait the full
	// interval for its first cleanup.
	s.webdavTokenSweepOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.webdavTokenSweepOnce(ctx)
		}
	}
}

func (s *Server) webdavTokenSweepOnce(ctx context.Context) {
	if s.webdavTokens == nil {
		return
	}
	sweepCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	n, err := s.webdavTokens.Sweep(sweepCtx)
	if err != nil && s.logger != nil {
		// Warn (not Error) — a transient kv lock during a sweep tick
		// resolves itself on the next run, and the cache-side delete
		// already happened so the verifier path stays correct.
		s.logger.Warn("webdav tokens: sweep failed", "err", err)
	}
	if n > 0 && s.logger != nil {
		s.logger.Info("webdav tokens: swept expired", "count", n)
	}
}
