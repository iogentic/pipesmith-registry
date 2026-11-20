// Package logship sends the JSON lines of the log files to log targets. A
// target is one row of the setting log_targets of state.sqlite (target.go):
// a kind, an address, the files it takes and the bounds of its queue.
//
// The Manager holds one bounded queue and one sender goroutine per target.
// Its tap, Ship, is the Options.Ship of internal/logs: it appends a line to
// the queue of every target that takes the kind of the line and returns at
// once. A full queue drops the new line and counts it. The sender hands
// batches to the adapter of the kind (adapter.go) and tries a batch at most
// 3 times. The manager reads the setting again every 5 s of its clock. When
// a row changes, each line reaches exactly one of the old and the new sender
// of the row (manager.go, reload).
//
// The lines of the logger pipesmith_registry.logship, the warnings and stop
// lines of this package, are never queued.
//
// The package imports internal/logs for its line type and no other package
// of this module.
package logship
