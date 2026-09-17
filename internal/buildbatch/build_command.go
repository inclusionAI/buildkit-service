package buildbatch

import (
	"fmt"
	"time"
)

func runBuildCommand(opts options) error {
	if opts.ctx == nil {
		resetCommandStartTime(time.Now())
	}

	if len(opts.addrs) == 0 {
		return fmt.Errorf("--addrs is required")
	}

	buildModes, err := resolveBuildModes(opts.oci, opts.bothFormats)
	if err != nil {
		return err
	}
	if opts.imageDirs != "" {
		return runBuildCommandStreamingImageDirs(opts, buildModes)
	}

	return runBuildCommandBatch(opts, buildModes)
}
