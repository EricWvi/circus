//go:build race

package calls_test

// raceEnabled selects cheaper defaults for long randomized tests under -race.
const raceEnabled = true
