package main

import (
	"github.com/nyaruka/archiver/v26/cmd"
	"github.com/nyaruka/archiver/v26/runtime"
)

var (
	// overridden at build time via -ldflags "-X main.version=... -X main.date=..."
	version = "dev"
	date    = "unknown"
)

func main() {
	cmd.Run(cmd.Job(runtime.NewDefaultConfig(), version, date, cmd.LogHandler()))
}
