//go:build race

package cli

// raceEnabled reports a -race build, where every step runs several times
// slower, so a test bounding wall-clock time has nothing to measure.
const raceEnabled = true
