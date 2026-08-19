// Command tln-plugin serves the Tln runtime. It compiles in whatever plugins
// bundledPlugins() returns — generated from a mod.tln by `cmd/bundle` at build
// time (see the Makefile). With no mod.tln the generated default returns none,
// so a plain `go build` yields the no-extensions binary. This repo declares no
// plugin itself; the list is supplied externally (e.g. Core writes mod.tln into
// the clone from config.yaml's `bundle:`).
package main

import (
	"log/slog"
	"os"

	"github.com/opentalon/tln-plugin/runtime"
)

func main() {
	if err := runtime.Serve(bundledPlugins()...); err != nil {
		slog.Error("tln-plugin: serve", "error", err)
		os.Exit(1)
	}
}
