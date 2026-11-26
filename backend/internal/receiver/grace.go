package receiver

import "time"

const controlCloseGrace = 5 * time.Second

type disconnectTimer interface {
	Stop() bool
}

func (r *Receiver) cancelDisconnectLocked(remote string) {
	if cs := r.sessions[remote]; cs != nil {
		cs.closeVersion++
		if cs.closeTimer != nil {
			cs.closeTimer.Stop()
			cs.closeTimer = nil
		}
	}
}

func (r *Receiver) cancelAllDisconnectsLocked() {
	for remote := range r.sessions {
		r.cancelDisconnectLocked(remote)
	}
}

func (r *Receiver) scheduleDisconnectLocked(remote string, cs *clientSession) {
	cs.closeVersion++
	version := cs.closeVersion
	delay := r.controlGrace
	if delay <= 0 {
		delay = controlCloseGrace
	}
	after := r.afterFunc
	if after == nil {
		after = func(d time.Duration, f func()) disconnectTimer { return time.AfterFunc(d, f) }
	}
	cs.closeTimer = after(delay, func() {
		r.transitionMu.Lock()
		defer r.transitionMu.Unlock()
		r.mu.Lock()
		if r.sessions[remote] != cs || cs.closeVersion != version || cs.closeTimer == nil {
			r.mu.Unlock()
			return
		}
		cs.closeTimer = nil
		delete(r.sessions, remote)
		owner := r.activeConn == remote
		var fx mediaEffects
		if owner {
			r.cleanStop.Store(true)
			fx = r.dropMediaLocked()
			r.recordStateLocked(StatusIdle, "control connection closed")
		}
		r.mu.Unlock()
		if owner {
			r.applyEffects(fx)
			r.flushState()
		}
	})
}
