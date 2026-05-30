package main

import (
	"os"

	"spawnery/internal/stubagent"
)

func main() {
	if err := stubagent.Run(os.Stdin, os.Stdout); err != nil {
		os.Exit(0) // EOF on stdin close is normal teardown
	}
}
