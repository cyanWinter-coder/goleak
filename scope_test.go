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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak/internal/stack"
)

// blockOn returns a channel that keeps a goroutine alive until it is closed.
func blockOn() chan struct{} {
	return make(chan struct{})
}

// waitForGoroutine blocks until a live goroutine whose "created by" function
// is, or is nested inside, the given enclosing function is observed. Closures
// launched with `go func()` are reported as "enclosing.funcN", so matching is
// by prefix.
func waitForGoroutine(t *testing.T, createdBy string) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, s := range stack.All() {
			if cb := s.CreatedBy(); cb == createdBy || strings.HasPrefix(cb, createdBy+".") {
				return true
			}
		}
		return false
	}, time.Second, time.Millisecond,
		"goroutine created by %s never appeared", createdBy)
}

func TestScopeNoLeaks(t *testing.T) {
	s := StartScope("clean")
	err := s.End(nil, testOptions())
	require.NoError(t, err)
}

func TestScopeFindsLeakCreatedWithinScope(t *testing.T) {
	s := StartScope("leaky")
	wait := blockOn()
	go func() { <-wait }()
	waitForGoroutine(t, "go.uber.org/goleak.TestScopeFindsLeakCreatedWithinScope")

	err := s.End(nil, testOptions())
	close(wait)

	require.Error(t, err)
	var scopeErr *ScopeError
	require.ErrorAs(t, err, &scopeErr)
	assert.Equal(t, "leaky", scopeErr.Scope)
	require.Len(t, scopeErr.Leaks, 1)
	assert.Contains(t, err.Error(), "owning scope: leaky")
	assert.Contains(t, err.Error(), "TestScopeFindsLeakCreatedWithinScope")
	assert.Contains(t, err.Error(), "created by")
}

func TestScopeIgnoresPreExistingGoroutine(t *testing.T) {
	// A goroutine alive before the scope started is part of the baseline and
	// must not be attributed to the scope even if it stays alive throughout.
	pre := blockOn()
	go func() { <-pre }()
	waitForGoroutine(t, "go.uber.org/goleak.TestScopeIgnoresPreExistingGoroutine")

	s := StartScope("with-baseline")
	err := s.End(nil, testOptions())
	close(pre)

	require.NoError(t, err)
}

func TestScopeIgnoresGoroutineThatExits(t *testing.T) {
	s := StartScope("exits")
	done := make(chan struct{})
	go func() { close(done) }()
	<-done

	require.NoError(t, s.End(nil))
}

func TestScopeNestedAttributesToInnermost(t *testing.T) {
	outer := StartScope("outer")

	inner := StartScope("inner")
	innerWait := blockOn()
	go func() { <-innerWait }()
	waitForGoroutine(t, "go.uber.org/goleak.TestScopeNestedAttributesToInnermost")

	// The inner scope owns the goroutine; the outer scope must not see it.
	innerErr := inner.End(nil, testOptions())
	close(innerWait)
	require.Error(t, innerErr)
	assert.Contains(t, innerErr.Error(), `scope "inner"`)

	// Once the goroutine is gone and the inner scope reported it, the outer
	// scope has nothing left to report.
	require.NoError(t, outer.End(nil))
}

func TestScopeNestedOuterReportsItsOwnLeak(t *testing.T) {
	outer := StartScope("outer2")
	outerWait := blockOn()
	go func() { <-outerWait }()
	waitForGoroutine(t, "go.uber.org/goleak.TestScopeNestedOuterReportsItsOwnLeak")

	inner := StartScope("inner2")
	innerDone := make(chan struct{})
	go func() { close(innerDone) }()
	<-innerDone
	require.NoError(t, inner.End(nil))

	err := outer.End(nil, testOptions())
	close(outerWait)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `scope "outer2"`)
}

func TestScopeNoDoubleReporting(t *testing.T) {
	// Two scopes on the same goroutine must never report the same leaked
	// goroutine twice, even when the outer one ends while it is still alive.
	outer := StartScope("double-outer")
	inner := StartScope("double-inner")

	wait := blockOn()
	go func() { <-wait }()
	waitForGoroutine(t, "go.uber.org/goleak.TestScopeNoDoubleReporting")

	innerErr := inner.End(nil, testOptions())
	require.Error(t, innerErr)

	outerErr := outer.End(nil, testOptions())
	close(wait)
	require.NoError(t, outerErr, "goroutine already reported by inner scope")
}

func TestScopeGracePeriodRetries(t *testing.T) {
	s := StartScope("grace")
	wait := blockOn()
	go func() { <-wait }()
	waitForGoroutine(t, "go.uber.org/goleak.TestScopeGracePeriodRetries")

	// Unblock from another goroutine during the retry window.
	go func() {
		time.Sleep(5 * time.Millisecond)
		close(wait)
	}()

	require.NoError(t, s.End(nil))
}

func TestScopeConcurrency(t *testing.T) {
	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			s := StartScope(fmt.Sprintf("parallel-%d", i))
			wait := blockOn()
			started := make(chan struct{})
			go func() {
				close(started)
				<-wait
			}()
			<-started // guarantee this scope's own goroutine is live before End
			errs[i] = s.End(nil, testOptions())
			close(wait)
		}()
	}
	wg.Wait()

	// Every scope must detect exactly its own leaked goroutine.
	for i, err := range errs {
		require.Error(t, err, "scope %d should find its leak", i)
		var scopeErr *ScopeError
		require.ErrorAs(t, err, &scopeErr)
		assert.Equal(t, fmt.Sprintf("parallel-%d", i), scopeErr.Scope)
		assert.Len(t, scopeErr.Leaks, 1)
	}
}

func TestScopePanicStillCleansUp(t *testing.T) {
	wait := blockOn()
	func() {
		defer func() {
			require.NotNil(t, recover(), "expected panic to propagate")
			close(wait)
		}()
		VerifyScope(nil, "panicking", func() {
			go func() { <-wait }()
			waitForGoroutine(t, "go.uber.org/goleak.TestScopePanicStillCleansUp")
			panic("boom")
		}, testOptions())
	}()

	// The registry must not retain the abandoned scope.
	scopeRegistry.mu.Lock()
	for _, sc := range scopeRegistry.open {
		assert.NotEqual(t, "panicking", sc.name, "scope should be removed after panic")
	}
	scopeRegistry.mu.Unlock()
}

func TestScopeCloseWithoutCheck(t *testing.T) {
	s := StartScope("closed")
	wait := blockOn()
	go func() { <-wait }()
	waitForGoroutine(t, "go.uber.org/goleak.TestScopeCloseWithoutCheck")

	s.Close()
	s.Close() // idempotent
	close(wait)

	scopeRegistry.mu.Lock()
	_, open := scopeRegistry.open[s.id]
	scopeRegistry.mu.Unlock()
	assert.False(t, open)
}

func TestScopeEndIdempotent(t *testing.T) {
	s := StartScope("idempotent")
	require.NoError(t, s.End(nil, testOptions()))
	require.NoError(t, s.End(nil, testOptions()))
}

func TestScopeNilReceiver(t *testing.T) {
	var s *Scope
	assert.NotPanics(t, func() {
		s.Close()
		assert.NoError(t, s.End(nil))
		assert.Empty(t, s.Name())
	})
}

func TestScopeRejectsInvalidOptions(t *testing.T) {
	s := StartScope("bad-options")
	err := s.End(nil, Cleanup(func(int) {}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Cleanup")

	s2 := StartScope("bad-options-2")
	err = s2.End(nil, RunOnFailure())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RunOnFailure")
}

func TestVerifyScopeReportsToT(t *testing.T) {
	ft := &fakeT{}
	wait := blockOn()
	VerifyScope(ft, "helper", func() {
		go func() { <-wait }()
		waitForGoroutine(t, "go.uber.org/goleak.TestVerifyScopeReportsToT")
	}, testOptions())
	close(wait)

	require.NotEmpty(t, ft.errors)
	assert.Contains(t, ft.errors[0], `scope "helper"`)
}

func TestVerifyScopeClean(t *testing.T) {
	ft := &fakeT{}
	VerifyScope(ft, "helper-clean", func() {
		done := make(chan struct{})
		go func() { close(done) }()
		<-done
	})
	assert.Empty(t, ft.errors)
}

func TestScopeErrorDoesNotLeakArgumentValues(t *testing.T) {
	secret := "super-secret-parameter-value-12345"
	s := StartScope("no-secrets")
	wait := blockOn()
	go func(secretArg string) { <-wait }(secret)
	waitForGoroutine(t, "go.uber.org/goleak.TestScopeErrorDoesNotLeakArgumentValues")

	err := s.End(nil, testOptions())
	close(wait)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
}

func TestScopeRestoresParentOnEnd(t *testing.T) {
	outer := StartScope("parent-restore")
	inner := StartScope("child-restore")
	inner.Close()

	scopeRegistry.mu.Lock()
	got := scopeRegistry.gScopes[stack.Current().ID()]
	scopeRegistry.mu.Unlock()
	assert.Same(t, outer, got, "innermost scope should revert to outer")

	outer.Close()
}

// TestScopeParallelSubtests is the motivating use case: parallel subtests that
// each leak a goroutine must attribute that goroutine to their own scope,
// instead of cross-contaminating each other the way a plain VerifyNone would.
func TestScopeParallelSubtests(t *testing.T) {
	for i := 0; i < 4; i++ {
		i := i
		t.Run(fmt.Sprintf("parallel-%d", i), func(t *testing.T) {
			t.Parallel()

			s := StartScope(t.Name())
			wait := blockOn()
			started := make(chan struct{})
			go func() {
				close(started)
				<-wait
			}()
			<-started

			err := s.End(nil, testOptions())
			close(wait)

			var scopeErr *ScopeError
			require.ErrorAs(t, err, &scopeErr)
			assert.Equal(t, t.Name(), scopeErr.Scope)
			assert.Len(t, scopeErr.Leaks, 1, "scope must report only its own goroutine")
		})
	}
}

func TestScopeRespectsExistingIgnoreRules(t *testing.T) {
	// The same ignore options honored by Find must be honored inside a scope.
	t.Run("IgnoreCreatedBy", func(t *testing.T) {
		s := StartScope("ignored-created-by")
		wait := blockOn()
		go func() { <-wait }()
		waitForGoroutine(t, "go.uber.org/goleak.TestScopeRespectsExistingIgnoreRules")

		// A `go func()` closure is reported as created by the enclosing
		// function's ".func1" symbol.
		err := s.End(nil,
			IgnoreCreatedBy("go.uber.org/goleak.TestScopeRespectsExistingIgnoreRules.func1"),
			testOptions())
		close(wait)
		assert.NoError(t, err, "goroutine ignored by creator should not be reported")
	})

	t.Run("IgnoreTopFunction", func(t *testing.T) {
		s := StartScope("ignored-top")
		bg := startBlockedG()

		err := s.End(nil,
			IgnoreTopFunction("go.uber.org/goleak.(*blockedG).block"),
			testOptions())
		bg.unblock()
		assert.NoError(t, err, "goroutine ignored by top function should not be reported")
	})
}

func TestScopeAttributesViaCreatorID(t *testing.T) {
	// A goroutine spawned from a nested goroutine (not the scope owner
	// directly) must still be attributed through the creator-ID chain.
	s := StartScope("creator-chain")
	started := make(chan struct{})
	wait := blockOn()
	go func() {
		// Second-level goroutine: its creator is the goroutine above.
		go func() {
			close(started)
			<-wait
		}()
		<-wait
	}()
	<-started

	err := s.End(nil, testOptions())
	close(wait)

	var scopeErr *ScopeError
	require.ErrorAs(t, err, &scopeErr)
	require.NotEmpty(t, scopeErr.Leaks)
	// Every reported goroutine must carry a creator ID enabling the chain walk.
	for _, st := range scopeErr.Leaks {
		assert.NotZero(t, st.CreatedByID(), "leak should have a resolvable creator goroutine")
		assert.NotEmpty(t, st.CreatedBy(), "leak should report its creation site")
	}
}
