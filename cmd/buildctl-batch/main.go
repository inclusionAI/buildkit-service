package main

import (
	"os"
	"time"

	"github.com/inclusionAI/buildkit-service/internal/buildbatch"
)

func main() {
	buildbatch.ResetCommandStartTime(time.Now())
	app := newCLIApp()
	if err := app.Run(os.Args); err != nil {
		buildbatch.LogError("%v", err)
		os.Exit(1)
	}
}
