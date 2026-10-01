//go:build !goexperiment.runtimesecret

// Package secret runs key translation that touches secret text under
// runtime/secret when the toolchain provides it (GOEXPERIMENT=runtimesecret),
// and plainly otherwise.
package secret

// Do runs f. Without GOEXPERIMENT=runtimesecret there is nothing to erase
// beyond what the caller wipes itself.
func Do(f func()) { f() }
