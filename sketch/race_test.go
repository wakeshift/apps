//go:build race

package sketch

// raceSlowdown is how much slower code runs under the race detector, for
// tests that time it.
const raceSlowdown = 20
