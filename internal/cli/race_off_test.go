//go:build perf && !race

package cli

// raceEnabled reports a -race build; see race_on_test.go.
const raceEnabled = false
