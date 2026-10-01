//go:build goexperiment.runtimesecret

// Package secret runs key translation that touches secret text under
// runtime/secret when the toolchain provides it (GOEXPERIMENT=runtimesecret),
// and plainly otherwise.
package secret

import "runtime/secret"

// Do runs f with its registers, stack and heap temporaries erased afterwards.
func Do(f func()) { secret.Do(f) }
