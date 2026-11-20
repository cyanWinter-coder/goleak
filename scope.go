// Copyright (c) 2026 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package goleak

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.uber.org/goleak/internal/stack"
)

// scopeRegistry tracks all open scopes process-wide so that goroutines can be
// attributed to a scope even when tests, or scopes within a test, run
// concurrently. All access is guarded by mu.
var scopeRegistry = struct {
	mu sync.Mutex

	// gScopes maps the ID of each goroutine that has an open scope to the
	// innermost such scope, i.e. the nearest owner of any goroutine it spawns.
	gScopes map[int]*Scope

	// open holds every scope that has not yet been ended or closed, keyed by
	// its unique sequence number.
	open map[int]*Scope

	// claimed holds goroutine IDs that have already been reported as leaks by
	// an ended scope, so the same goroutine is never reported twice.
	claimed map[int]bool

	seq int
}{
	gScopes: make(map[int]*Scope),
	open:    make(map[int]*Scope),
	claimed: make(map[int]bool),
}

// Scope is a named snapshot region that attributes leaked goroutines to the
// region in which they were started.
//
// A scope records a baseline of live goroutines when it starts. When it ends
// it inspects only goroutines that were started after the baseline was taken
// and are still alive, attributing each one to the innermost open scope that
// owns it. This makes scopes safe to use from parallel tests, where
// [VerifyNone] cannot tell one test's goroutines from another's.
//
// A Scope must be ended exactly once, either through [Scope.End] (which runs
// the leak check) or [Scope.Close] (which does not). End and Close are both
// idempotent: calling either one more than once has no additional effect.
// The [StartScope] helper and [VerifyScope] function cover the common
// defer-based patterns.
type Scope struct {
	name string
	id   int // unique sequence number used as a registry key

	// parent is the scope that was innermost on the starting goroutine,
	// if any. The parent chain is fixed for the lifetime of the scope.
	parent *Scope

	// ownerG is the goroutine that started this scope.
	ownerG int

	// baseline is the set of goroutine IDs that were alive when the scope
	// started. Anything not in this set was created during the scope.
	baseline map[int]bool

	once  sync.Once
	ended bool
}

// StartScope opens a named scope, recording the current set of live
// goroutines as its baseline. The returned Scope must be ended with
// [Scope.End] or [Scope.Close] when the region it covers is complete:
//
//	s := goleak.StartScope("TestFoo/worker")
//	defer s.End(t)
//
// If the calling goroutine already has an open scope, the new scope is
// nested inside it: goroutines are attributed to the innermost scope.
func StartScope(name string) *Scope {
	cur := stack.Current().ID()

	scopeRegistry.mu.Lock()
	defer scopeRegistry.mu.Unlock()

	scopeRegistry.seq++
	s := &Scope{
		name:     name,
		id:       scopeRegistry.seq,
		ownerG:   cur,
		parent:   scopeRegistry.gScopes[cur],
		baseline: make(map[int]bool),
	}
	for _, st := range stack.All() {
		s.baseline[st.ID()] = true
	}

	scopeRegistry.open[s.id] = s
	scopeRegistry.gScopes[cur] = s
	return s
}

// Name returns the name the scope was started with.
func (s *Scope) Name() string {
	if s == nil {
		return ""
	}
	return s.name
}

// End runs the leak check for the scope and marks it finished.
//
// Only goroutines that were created while the scope was open and that are
// still alive after the configured grace period and retries are considered.
// Each such goroutine is attributed to its innermost owning scope, so a
// goroutine created inside a nested scope is never reported by an ancestor.
// A goroutine that was already reported by another scope is not reported
// again.
//
// If leaks are found, the error is reported on t and also returned. End is
// idempotent and safe to defer; a scope ended with a panic in flight is
// cleaned up deterministically and the panic is not swallowed.
func (s *Scope) End(t TestingT, options ...Option) error {
	if s == nil {
		return nil
	}
	if h, ok := t.(testHelper); ok {
		h.Helper()
	}

	var err error
	s.once.Do(func() {
		// Check while the scope is still registered: creator-chain
		// attribution resolves the innermost open scope on the creator
		// goroutine, so removing the scope first would make its own
		// goroutines look ownerless.
		err = s.check(options...)
		s.finish()
	})
	if err != nil && t != nil {
		t.Error(err)
	}
	return err
}

// Close discards the scope without running a leak check. It is intended for
// snapshots that were taken but are no longer needed. Like [Scope.End] it is
// idempotent and deterministically removes the scope from the registry.
func (s *Scope) Close() {
	if s == nil {
		return
	}
	s.once.Do(s.finish)
}

// finish removes the scope from the registry. It runs at most once.
func (s *Scope) finish() {
	scopeRegistry.mu.Lock()
	defer scopeRegistry.mu.Unlock()

	s.ended = true
	delete(scopeRegistry.open, s.id)

	// Restore the nearest enclosing scope as the innermost scope for this
	// goroutine, but only if this scope is still the one registered: a
	// nested scope started later on the same goroutine may have replaced it.
	if scopeRegistry.gScopes[s.ownerG] == s {
		if s.parent != nil && !s.parent.ended {
			scopeRegistry.gScopes[s.ownerG] = s.parent
		} else {
			delete(scopeRegistry.gScopes, s.ownerG)
		}
	}
}

// check performs the grace-period retries and builds the leak error.
func (s *Scope) check(options ...Option) error {
	cur := stack.Current().ID()
	opts := buildOpts(options...)
	if opts.cleanup != nil {
		return fmt.Errorf("goleak: Cleanup can only be passed to VerifyNone or VerifyTestMain")
	}
	if opts.runOnFailure {
		return fmt.Errorf("goleak: RunOnFailure can only be passed to VerifyTestMain")
	}

	var leaks []stack.Stack
	retry := true
	for i := 0; retry; i++ {
		leaks = s.snapshotLeaks(cur, opts)
		if len(leaks) == 0 {
			return nil
		}
		retry = opts.retry(i)
	}

	// Claim the survivors under the lock so concurrent or nested scopes
	// never report the same goroutine twice.
	scopeRegistry.mu.Lock()
	final := leaks[:0]
	for _, st := range leaks {
		if scopeRegistry.claimed[st.ID()] {
			continue
		}
		scopeRegistry.claimed[st.ID()] = true
		final = append(final, st)
	}
	scopeRegistry.mu.Unlock()

	if len(final) == 0 {
		return nil
	}
	return newScopeError(s.name, final)
}

// snapshotLeaks returns the live, unfiltered goroutines attributed to this
// scope that have not already been claimed.
func (s *Scope) snapshotLeaks(cur int, opts *opts) []stack.Stack {
	all := stack.All()
	byID := make(map[int]stack.Stack, len(all))
	for _, st := range all {
		byID[st.ID()] = st
	}

	scopeRegistry.mu.Lock()
	defer scopeRegistry.mu.Unlock()

	// Map every goroutine that currently owns an open scope to that scope.
	// Deriving this from the open-scope set (rather than from gScopes alone)
	// makes attribution stable while a scope is inside End: the scope stays
	// open until its check completes, so its owner goroutine remains
	// identifiable to concurrent scopes even though their scans interleave.
	owners := make(map[int]*Scope, len(scopeRegistry.open))
	for _, sc := range scopeRegistry.open {
		if existing, ok := owners[sc.ownerG]; !ok || isDescendant(sc, existing) {
			// Multiple open scopes on one goroutine are nested; keep the
			// innermost (deepest) one as the nearest owner.
			owners[sc.ownerG] = sc
		}
	}

	leaks := make([]stack.Stack, 0)
	for _, st := range all {
		id := st.ID()
		if id == cur || s.baseline[id] || scopeRegistry.claimed[id] {
			continue
		}
		if opts.filter(st) {
			continue
		}
		if !s.owns(st, byID, owners) {
			continue
		}
		leaks = append(leaks, st)
	}
	return leaks
}

// isDescendant reports whether child is nested inside ancestor.
func isDescendant(child, ancestor *Scope) bool {
	for p := child.parent; p != nil; p = p.parent {
		if p == ancestor {
			return true
		}
	}
	return false
}

// owns reports whether goroutine st should be attributed to this scope.
// Caller must hold scopeRegistry.mu.
func (s *Scope) owns(st stack.Stack, byID map[int]stack.Stack, owners map[int]*Scope) bool {
	// Walk the creator chain to the goroutine that started this one. The
	// innermost open scope on that goroutine is the nearest owner.
	owner := s.creatorScope(st, byID, owners)
	if owner == nil {
		// The chain leads to no open scope. Two cases:
		//
		//   - pre-Go 1.21 traces carry no creator ID at all, so the chain
		//     cannot be walked; attribute to the only scope that can see the
		//     goroutine through its baseline filter, i.e. this one.
		//   - the creator belongs to an unrelated, already-ended branch.
		//
		// Goroutines whose creator still owns a live (sibling) scope are
		// handled above and never reach this fallback, so this cannot claim
		// another live scope's goroutine.
		owner = s
	}
	return owner == s
}

// creatorScope resolves the innermost open scope on the goroutine's creator
// chain. It returns nil when the chain does not lead to any open scope.
// Caller must hold scopeRegistry.mu.
func (s *Scope) creatorScope(st stack.Stack, byID map[int]stack.Stack, owners map[int]*Scope) *Scope {
	seen := map[int]bool{}
	creatorID := st.CreatedByID()
	for creatorID != 0 && !seen[creatorID] {
		seen[creatorID] = true

		// A still-open scope on the creator goroutine is the nearest owner.
		// If it is unrelated to this scope (e.g. a parallel sibling test),
		// returning it makes owns() reject the goroutine, so this scope never
		// claims another branch's goroutines.
		if owner, ok := owners[creatorID]; ok {
			return owner
		}

		parent, ok := byID[creatorID]
		if !ok {
			return nil
		}
		creatorID = parent.CreatedByID()
	}
	return nil
}

// ScopeError describes goroutines leaked within a single scope.
type ScopeError struct {
	// Scope is the name of the scope the goroutines are attributed to.
	Scope string

	// Leaks are the leaked goroutine stacks, sorted by goroutine ID.
	Leaks []stack.Stack
}

func newScopeError(name string, leaks []stack.Stack) *ScopeError {
	sort.Slice(leaks, func(i, j int) bool {
		return leaks[i].ID() < leaks[j].ID()
	})
	return &ScopeError{Scope: name, Leaks: leaks}
}

// Error implements the error interface.
//
// The message includes each goroutine's stable stack summary, its creation
// site and its owning scope. It never includes test argument values: stack
// traces carry function and file names only, not runtime data.
func (e *ScopeError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "goleak: found %d unexpected goroutine(s) in scope %q:\n",
		len(e.Leaks), e.Scope)
	for _, st := range e.Leaks {
		fmt.Fprintf(&b, "Goroutine %d in state %s, with %s on top of the stack:\n",
			st.ID(), st.State(), st.FirstFunction())
		b.WriteString(st.Full())
		if createdBy := st.CreatedBy(); createdBy != "" {
			fmt.Fprintf(&b, "created by %s", createdBy)
			if id := st.CreatedByID(); id != 0 {
				fmt.Fprintf(&b, " in goroutine %d", id)
			}
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "owning scope: %s\n", e.Scope)
	}
	return b.String()
}

// VerifyScope opens a scope with the given name, runs fn, and ends the scope
// when fn returns, reporting any leaks on t. It is a convenience wrapper
// around [StartScope] and [Scope.End]:
//
//	goleak.VerifyScope(t, "worker-pool", func() {
//	    // code under test
//	})
//
// If fn panics, the deferred scope teardown still runs deterministically and
// the panic continues to propagate after the scope's bookkeeping is removed.
// The grace period and retry behavior can be customized with the same
// [Option] values accepted by [Find], except [Cleanup] and [RunOnFailure].
func VerifyScope(t TestingT, name string, fn func(), options ...Option) {
	if h, ok := t.(testHelper); ok {
		h.Helper()
	}

	s := StartScope(name)
	// End is idempotent and always removes the scope from the registry,
	// including when fn panics: defers run during panic unwinding before the
	// panic propagates to the testing package.
	defer s.End(t, options...)

	if fn != nil {
		fn()
	}
}
