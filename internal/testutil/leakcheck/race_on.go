//go:build race

package leakcheck

// The race detector multiplies memory use; measured peaks grow about 3x.
const raceMemoryFactor = 3
