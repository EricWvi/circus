//go:build race

package ledger

// raceEnabled shrinks the heaviest tests under the race detector, which
// slows SQLite and Git work many times over.
const raceEnabled = true
