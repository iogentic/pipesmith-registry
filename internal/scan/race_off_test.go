//go:build !race

package scan_test

// raceOn is true when the tests run with the race detector, which slows
// every call and makes a time bound meaningless.
const raceOn = false
