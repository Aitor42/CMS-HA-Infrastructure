package main

import (
	"os"

	"github.com/Aitor42/CMS-HA-Infrastructure/cmd/cms-ha/root"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
)

// These are set by the Makefile and by GoReleaser through -ldflags. Keeping
// the defaults useful means a plain `go build` still reports something sane.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	root.SetBuildInfo(version, commit, date)
	if err := root.Execute(); err != nil {
		// Failing commands return their error here instead of calling os.Exit,
		// so deferred cleanups (e.g. pool.Close()) run before termination.
		logging.Error("%v", err)
		os.Exit(1)
	}
}
