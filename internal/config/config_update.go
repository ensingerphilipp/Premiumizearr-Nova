package config

import (
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// The swap mutex is registered process-wide rather than stored on the
// Config struct: UpdateConfig replaces the struct in place on every call,
// and a struct field that service goroutines read at runtime (to obtain
// the lock itself) would be a data race against that replacement. One
// daemon owns one Config, so a process-wide registration matches the
// product shape; each test registers its own mutex the same way.
var (
	registeredUpdateMuMu  sync.RWMutex
	registeredUpdateMuRef *sync.RWMutex
)

// SetUpdateMu registers the mutex UpdateConfig takes around the in-place
// struct swap, so the owning service's goroutines can read config fields
// under the same mutex's read lock. A nil registration (the default)
// leaves the swap unlocked. Call it once, before any goroutine starts
// reading the config.
func SetUpdateMu(mu *sync.RWMutex) {
	registeredUpdateMuMu.Lock()
	registeredUpdateMuRef = mu
	registeredUpdateMuMu.Unlock()
}

// UpdateMu returns the registered swap mutex (or nil when none was
// registered). Services that share the config but own a different lock -
// the transfer manager and the web handlers - read config fields under
// this mutex's read lock so their reads cannot interleave with the
// in-place struct swap.
func UpdateMu() *sync.RWMutex {
	registeredUpdateMuMu.RLock()
	defer registeredUpdateMuMu.RUnlock()
	return registeredUpdateMuRef
}

// reconfigRequest is one pending asynchronous reconfiguration of a Config
// instance. cfg is the live (swapped-in) config whose private fields and
// appCallback are authoritative; oldest is the pre-swap state of the FIRST
// save batched since the previous callback ran, so the callback still sees
// the oldest state it missed (change detection stays correct when several
// swaps coalesce into one callback invocation).
type reconfigRequest struct {
	cfg    *Config
	oldest Config
}

// The serialized reconfiguration worker. One process-wide goroutine owns
// all service reconfiguration: saves never wait on the reconfiguration
// they trigger (finding C-4) and a stale callback can never apply an
// older state on top of a newer commit - each invocation applies the
// oldest missed state to the newest committed state (finding S-28).
var (
	reconfigMu           sync.Mutex
	reconfigCh           chan struct{}
	reconfigWorkerOnce   sync.Once
	reconfigPend         = make(map[*Config]reconfigRequest)
	reconfigPendingGen   uint64
	reconfigProcessedGen uint64
)

// queueReconfig enqueues an asynchronous reconfiguration of c, whose live
// state has just been swapped from old to the new config. Requests for the
// same Config instance coalesce into one pending entry keeping the oldest
// missed state; the callback re-reads the newest committed state at run
// time, so coalescing never loses a change. A new entry bumps the pending
// generation so WaitReconfigIdle observes it.
func queueReconfig(c *Config, old Config) {
	reconfigMu.Lock()
	defer reconfigMu.Unlock()

	if reconfigCh == nil {
		reconfigCh = make(chan struct{}, 1)
	}
	if _, ok := reconfigPend[c]; !ok {
		reconfigPend[c] = reconfigRequest{cfg: c, oldest: old}
		reconfigPendingGen++
	}
	reconfigWorkerOnce.Do(func() {
		go reconfigWorkerLoop(reconfigCh)
	})
	// Non-blocking signal: if a batch is already in flight the worker
	// re-signals itself after the batch when new work is pending.
	select {
	case reconfigCh <- struct{}{}:
	default:
	}
}

// reconfigWorkerLoop is the single process-wide serialized reconfiguration
// worker. It drains the pending map into a batch, applies each entry, then
// re-signals if more work arrived while the batch ran.
func reconfigWorkerLoop(ch chan struct{}) {
	for range ch {
		reconfigMu.Lock()
		batch := reconfigPend
		reconfigPend = make(map[*Config]reconfigRequest)
		reconfigMu.Unlock()

		for _, req := range batch {
			applyReconfig(req)
		}

		reconfigMu.Lock()
		reconfigProcessedGen = reconfigPendingGen
		needSignal := len(reconfigPend) > 0
		if needSignal {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
		reconfigMu.Unlock()
	}
}

// applyReconfig invokes the registered app callback for one pending
// reconfiguration. The callback runs outside the swap lock: service
// callbacks take that same mutex for their own state and an RWMutex is not
// reentrant, and they may perform long-running network I/O (or re-enter
// config reads), so holding the swap lock there would serialize unrelated
// saves and config reads (finding C-4). The callback receives the oldest
// missed state and the newest committed state at invocation time. A
// panicking callback is recovered so one bad save cannot kill the worker.
func applyReconfig(req reconfigRequest) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("config reconfiguration callback panicked: %v", r)
		}
	}()

	mu := UpdateMu()
	var cb AppCallback
	var current Config
	if mu != nil {
		mu.RLock()
		current = *req.cfg
		cb = req.cfg.appCallback
		mu.RUnlock()
	} else {
		current = *req.cfg
		cb = req.cfg.appCallback
	}
	if cb == nil {
		return
	}
	cb(req.oldest, current)
}

// WaitReconfigIdle blocks until the serialized reconfiguration worker has
// processed every enqueued save. It is a test drain barrier: it guarantees
// that the service reconfiguration triggered by UpdateConfig has completed
// before assertions on service state.
func WaitReconfigIdle() {
	for {
		reconfigMu.Lock()
		done := reconfigProcessedGen >= reconfigPendingGen
		reconfigMu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// UpdateConfig atomically replaces the live config with _newConfig,
// persists it, and enqueues the asynchronous service reconfiguration for
// the swap.
//
// The swap, the private-field carry-over, and the field-wise YAML marshal
// in Save all run under the registered swap lock, so a persisted config is
// always one complete update (finding C-17) and the config is safely stored
// before this call returns (finding C-4f). The call never blocks on the
// reconfiguration itself (finding C-4: the web endpoint must not hang while
// a service callback waits on premiumize.me): the reconfiguration runs on a
// single process-wide serialized worker that applies each callback
// invocation to the newest committed state, so a stale callback can never
// restore an older config state (finding S-28).
//
// The returned error is a config persistence failure; the in-memory swap
// is kept because the new state is valid and already committed.
func (c *Config) UpdateConfig(_newConfig Config) error {
	// An update body that omits Arrs decodes as a nil slice, which the API
	// would then serve back as "Arrs": null; normalize to an empty slice so
	// the array field always serializes as an array.
	if _newConfig.Arrs == nil {
		_newConfig.Arrs = []ArrConfig{}
	}

	// The swap lock is resolved through the process-wide registry, not a
	// struct field: the in-place swap below rewrites the whole struct, and
	// a field the service goroutines read at runtime to obtain the lock
	// itself would race that rewrite (the detector flags the equal-value
	// rewrite too).
	mu := UpdateMu()

	var oldConfig Config
	if mu != nil {
		mu.Lock()
		// The private fields are carried over while the swap lock is held:
		// they are read from the live struct, and an unlocked read would
		// race a concurrent swap.
		_newConfig.appCallback = c.appCallback
		_newConfig.altConfigLocation = c.altConfigLocation
		oldConfig = *c
		*c = _newConfig
		mu.Unlock()
	} else {
		_newConfig.appCallback = c.appCallback
		_newConfig.altConfigLocation = c.altConfigLocation
		oldConfig = *c
		*c = _newConfig
	}

	// Persist under the same read lock: Save marshals every field of *c,
	// and a concurrent swap must not interleave with the field-wise read or
	// a mixed config.yaml (new value for one field, old for another) is
	// written to disk. The persistence error is returned to the caller so
	// the web endpoint can report the failed save instead of claiming
	// success (finding C-4f); the in-memory swap is kept - the new state
	// is valid and already committed.
	if mu != nil {
		mu.RLock()
		defer mu.RUnlock()
	}
	if err := c.Save(); err != nil {
		log.Errorf("Failed to save config: %v", err)
		return err
	}

	// Reconfiguration is asynchronous: service callbacks (watcher path
	// updates, Arr folder resolution, transfer manager re-resolution) do
	// network I/O and must not hold the request or the swap lock.
	queueReconfig(c, oldConfig)

	return nil
}
