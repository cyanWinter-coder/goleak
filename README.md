# goleak [![GoDoc][doc-img]][doc] [![Build Status][ci-img]][ci] [![Coverage Status][cov-img]][cov]

Goroutine leak detector to help avoid Goroutine leaks.

## Installation

You can use `go get` to get the latest version:

`go get -u go.uber.org/goleak`

`goleak` also supports semver releases.

Note that go-leak only [supports][release] the two most recent minor versions of Go.

## Quick Start

To verify that there are no unexpected goroutines running at the end of a test:

```go
func TestA(t *testing.T) {
	defer goleak.VerifyNone(t)

	// test logic here.
}
```

Instead of checking for leaks at the end of every test, `goleak` can also be run
at the end of every test package by creating a `TestMain` function for your
package:

```go
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
```

### Note

For tests that use [t.Parallel](https://pkg.go.dev/testing#T.Parallel), `goleak` does
not know how to distinguish a leaky goroutine from tests that have not finished running.


```go
func TestA(t *testing.T) {
	tt := struct{
		name  	 string
		input 	 SomeType
		expected string
	}{
		// ...
	}

	for _, t := range tt {
		t.Run(t.name, func(t *testing.T) {
			t.Parallel() // <- goleak gets confused here!

			// ...
		}
	}
}
```
For such cases you should also defer to using `goleak.VerifyTestMain` as shown above.

### Named scopes for parallel tests

`VerifyNone` cannot tell a goroutine leaked by one test apart from the
goroutines of another test running at the same time. For that, use a named
**scope**. A scope records a baseline of live goroutines when it opens and, on
exit, checks only goroutines that were created during its lifetime and are
still alive — attributing each one to the innermost scope that owns it:

```go
func TestParallelWorkers(t *testing.T) {
	t.Parallel()

	s := goleak.StartScope(t.Name())
	defer s.End(t)

	// goroutines started here are attributed to this scope
}
```

Scopes may be nested. A goroutine created inside an inner scope is reported by
that inner scope only, never again by an ancestor. Scopes opened from different
goroutines (such as parallel subtests) are independent: each reports only its
own goroutines.

For the common open/run/check pattern, `VerifyScope` wraps the whole thing and
guarantees deterministic cleanup even if the function panics:

```go
goleak.VerifyScope(t, "worker-pool", func() {
	// code under test
})
```

A scope that is opened but should not be checked can be discarded with
`(*Scope).Close()`. Both `End` and `Close` are idempotent. The grace period and
retry behavior can be tuned with the same options as `Find` (except `Cleanup`
and `RunOnFailure`, which remain exclusive to `VerifyNone`/`VerifyTestMain`).

On Go 1.21 and newer, attribution follows the runtime's `created by ... in
goroutine N` information, so goroutines spawned indirectly through helper
goroutines are still assigned to the correct scope. Error reports include a
stable stack summary, the creation site and the owning scope, but never test
argument values (stack traces contain function and file names only).

## Determine Source of Package Leaks

When verifying leaks using `TestMain`, the leak test is only run once after all tests
have been run. This is typically enough to ensure there's no goroutines leaked from
tests, but when there are leaks, it's hard to determine which test is causing them.

You can use the following bash script to determine the source of the failing test:

```sh
# Create a test binary which will be used to run each test individually
$ go test -c -o tests

# Run each test individually, printing "." for successful tests, or the test name
# for failing tests.
$ for test in $(go test -list . | grep -E "^(Test|Example)"); do ./tests -test.run "^$test\$" &>/dev/null && echo -n "." || echo -e "\n$test failed"; done
```

This will only print names of failing tests which can be investigated individually. E.g.,

```
.....
TestLeakyTest failed
.......
```

## Stability

goleak is v1 and follows [SemVer](http://semver.org/) strictly.

No breaking changes will be made to exported APIs before 2.0.

[doc-img]: https://pkg.go.dev/badge/go.uber.org/goleak.svg
[doc]: https://pkg.go.dev/go.uber.org/goleak
[ci-img]: https://github.com/uber-go/goleak/actions/workflows/ci.yml/badge.svg
[ci]: https://github.com/uber-go/goleak/actions/workflows/ci.yml
[cov-img]: https://codecov.io/gh/uber-go/goleak/branch/master/graph/badge.svg
[cov]: https://codecov.io/gh/uber-go/goleak
[release]: https://go.dev/doc/devel/release#policy
