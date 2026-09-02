package main

import (
	"os"

	"github.com/Aitor42/CMS-HA-Infrastructure/cmd/cms-ha/root"
)

func main() {
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
