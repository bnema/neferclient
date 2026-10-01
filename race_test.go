package neferclient_test

import "testing"

// skipUnderRace skips allocation guards: the race detector randomly drops
// sync.Pool entries and instruments allocations, so counts are meaningless.
func skipUnderRace(t *testing.T) {
	t.Helper()
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under -race")
	}
}
