package proxy

import (
	"context"
	"fmt"
	"time"
)

func (p *proxyServer) tracef(format string, args ...any) {
	p.logger.Printf(format, args...)
}
func (p *proxyServer) ListenAndServe(parentCtx context.Context) error {
	held, err := acquireProxyLock(p.rootDir)
	if err != nil {
		return err
	}
	defer held.release()
	if err := clearSpawnMarkerAfterLock(p.rootDir); err != nil {
		return fmt.Errorf("clear proxy spawn marker: %w", err)
	}
	if changed, err := stopEpochChanged(p.rootDir, p.stopEpoch); err != nil {
		return fmt.Errorf("check proxy stop epoch after acquiring %s: %w", LockFileName, err)
	} else if changed {
		return fmt.Errorf("%w for %s: stop epoch advanced before startup", errStartInterrupted, p.rootDir)
	}
	run, err := startProxyRun(p, parentCtx, held)
	if err != nil {
		return err
	}
	defer run.close(p)
	return runProxyLoops(p, run)
}
func (p *proxyServer) idleWatcher(ctx context.Context) error {
	if p.idleTimeout <= 0 {
		<-ctx.Done()
		return nil
	}
	interval := p.idleTimeout / 4
	if interval < idleWatcherMinInterval {
		interval = idleWatcherMinInterval
	}
	p.tracef("idleWatcher start (timeout=%s, tick=%s)", p.idleTimeout, interval)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var idleSince time.Time
	for {
		select {
		case <-ctx.Done():
			p.tracef("idleWatcher exit (ctx done)")
			return nil
		case <-tick.C:
			if done, err := handleIdleTick(p, &idleSince); done {
				return err
			}
		}
	}
}
