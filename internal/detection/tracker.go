package detection

import (
	"sync"
	"time"
)

type Tracker struct {
	mu          sync.Mutex
	persistence time.Duration
	cooldown    time.Duration
	conditions  map[string]*conditionState
	resources   map[string]map[string]struct{}
	lastTrigger map[string]time.Time
}

type conditionState struct {
	failure   Failure
	firstSeen time.Time
	pending   bool
	triggered bool
}

func NewTracker(persistence, cooldown time.Duration) *Tracker {
	return &Tracker{
		persistence: persistence,
		cooldown:    cooldown,
		conditions:  make(map[string]*conditionState),
		resources:   make(map[string]map[string]struct{}),
		lastTrigger: make(map[string]time.Time),
	}
}

// Reconcile replaces current failures for one resource and reserves qualified
// failures until Complete reports whether durable workflow acceptance succeeded.
func (t *Tracker) Reconcile(resource string, failures []Failure, now time.Time) []Failure {
	t.mu.Lock()
	defer t.mu.Unlock()

	current := make(map[string]struct{}, len(failures))
	for _, failure := range failures {
		key := failureKey(failure)
		current[key] = struct{}{}
		state, ok := t.conditions[key]
		if !ok {
			state = &conditionState{failure: failure, firstSeen: now}
			t.conditions[key] = state
		} else {
			state.failure = failure
		}
	}
	for key := range t.resources[resource] {
		if _, ok := current[key]; !ok {
			delete(t.conditions, key)
		}
	}
	t.resources[resource] = current

	qualified := make([]Failure, 0, len(failures))
	for _, failure := range failures {
		key := failureKey(failure)
		state := t.conditions[key]
		if state.pending || state.triggered || now.Sub(state.firstSeen) < t.persistence {
			continue
		}
		if last, ok := t.lastTrigger[key]; ok && now.Sub(last) < t.cooldown {
			continue
		}
		state.pending = true
		qualified = append(qualified, state.failure)
	}
	return qualified
}

func (t *Tracker) Complete(failure Failure, accepted bool, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := failureKey(failure)
	state, ok := t.conditions[key]
	if !ok {
		return
	}
	state.pending = false
	if accepted {
		state.triggered = true
		t.lastTrigger[key] = now
	}
}

func failureKey(failure Failure) string {
	return failure.Namespace + "\x00" + failure.Kind + "\x00" + failure.Name + "\x00" + failure.Container + "\x00" + failure.Identity
}
