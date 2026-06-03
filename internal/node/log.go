package node

import (
	"log"
	"runtime/debug"
)

// logErr logs an error returned from one of our internal calls (the spawnlet Manager / runtime
// APIs) at warn level with a stack trace, so node-side failures that would otherwise be swallowed
// are diagnosable. `what` is a short context string (e.g. "openSession attach <spawnId>").
func logErr(what string, err error) {
	log.Printf("warn: %s: %v\n%s", what, err, debug.Stack())
}
