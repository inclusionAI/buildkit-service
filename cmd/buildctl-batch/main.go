package main

import (
	"os"
	"time"
)

func main() {
	resetCommandStartTime(time.Now())
	app := newCLIApp()
	if err := app.Run(os.Args); err != nil {
		logError("%v", err)
		os.Exit(1)
	}
}
