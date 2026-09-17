package main

import (
	"os"

	"github.com/inclusionAI/buildkit-service/internal/builddaemon"
)

func main() {
	if code := runCommand(os.Args, os.Stderr, builddaemon.Run); code != 0 {
		os.Exit(code)
	}
}
