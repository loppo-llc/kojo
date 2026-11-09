package agent

import "sync"

type fifoReservation struct {
	ready   <-chan struct{}
	done    chan struct{}
	once    sync.Once
	cleanup func()
}

func (r *fifoReservation) Wait() { <-r.ready }
func (r *fifoReservation) Release() {
	r.once.Do(func() {
		close(r.done)
		if r.cleanup != nil {
			r.cleanup()
		}
	})
}

// keyedFIFO reserves strict per-key execution order without blocking the
// caller. Reserve appends a ticket synchronously; workers wait on their ticket
// later, so an arrival callback can acknowledge queue admission without
// holding a global event loop or relying on sync.Mutex waiter ordering.
type keyedFIFO struct {
	mu    sync.Mutex
	tails map[string]chan struct{}
}

func (q *keyedFIFO) Reserve(id string) *fifoReservation {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.reserveLocked(id)
}

// ReservePair appends a turn and its optional handoff-arrival slot in one
// critical section. A later human turn therefore cannot land between them.
func (q *keyedFIFO) ReservePair(id string) (*fifoReservation, *fifoReservation) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.reserveLocked(id), q.reserveLocked(id)
}

// TryReserve takes a ticket only when id has no outstanding reservation —
// nothing running and nothing queued — and reports whether it did. The ticket
// is ready immediately; holding it keeps every later Reserve waiting.
func (q *keyedFIFO) TryReserve(id string) (*fifoReservation, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.tails[id] != nil {
		return nil, false
	}
	return q.reserveLocked(id), true
}

func (q *keyedFIFO) reserveLocked(id string) *fifoReservation {
	if q.tails == nil {
		q.tails = make(map[string]chan struct{})
	}
	ready := q.tails[id]
	if ready == nil {
		ready = make(chan struct{})
		close(ready)
	}
	done := make(chan struct{})
	q.tails[id] = done
	return &fifoReservation{ready: ready, done: done, cleanup: func() {
		q.mu.Lock()
		if q.tails[id] == done {
			delete(q.tails, id)
		}
		q.mu.Unlock()
	}}
}

// keyedMutex is a set of mutexes keyed by string id, created lazily on
// first use. The zero value is ready to use and safe for concurrent
// callers. It replaces the hand-rolled "guard mutex + map[string]*sync.Mutex"
// pattern used throughout this package for per-agent / per-group
// serialization.
//
// Entries are never reclaimed unless Drop is called: removing an entry
// while a goroutine is (or will be) locking the old mutex would let a
// new entry be created and silently break the serialization invariant.
// The leak is bounded by the number of distinct ids ever seen (one
// small *sync.Mutex each).
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

// get returns id's mutex, creating it (and the backing map) on first use.
func (k *keyedMutex) get(id string) *sync.Mutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = make(map[string]*sync.Mutex)
	}
	mu, ok := k.m[id]
	if !ok {
		mu = &sync.Mutex{}
		k.m[id] = mu
	}
	return mu
}

// Lock blocks until id's mutex is acquired and returns its unlock
// function. Callers should `defer release()` immediately.
func (k *keyedMutex) Lock(id string) func() {
	mu := k.get(id)
	mu.Lock()
	return mu.Unlock
}

// TryLock attempts to acquire id's mutex without blocking. It returns
// (unlock, true) on success, or (nil, false) if the mutex is already held.
func (k *keyedMutex) TryLock(id string) (func(), bool) {
	mu := k.get(id)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// Drop removes id's entry so the map doesn't grow without bound over the
// process lifetime. Safe to call for an unknown id. Only Drop an id that
// no goroutine is (or will be) locking.
func (k *keyedMutex) Drop(id string) {
	k.mu.Lock()
	delete(k.m, id)
	k.mu.Unlock()
}

// keyedRWMutex is the read/write variant of keyedMutex: many concurrent
// RLock holders or a single exclusive Lock holder per id. Same lazy
// creation and leak-by-design semantics.
type keyedRWMutex struct {
	mu sync.Mutex
	m  map[string]*sync.RWMutex
}

func (k *keyedRWMutex) get(id string) *sync.RWMutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = make(map[string]*sync.RWMutex)
	}
	mu, ok := k.m[id]
	if !ok {
		mu = &sync.RWMutex{}
		k.m[id] = mu
	}
	return mu
}

// Lock acquires id's write lock and returns its unlock function.
func (k *keyedRWMutex) Lock(id string) func() {
	mu := k.get(id)
	mu.Lock()
	return mu.Unlock
}

// RLock acquires id's read lock and returns its runlock function.
func (k *keyedRWMutex) RLock(id string) func() {
	mu := k.get(id)
	mu.RLock()
	return mu.RUnlock
}
