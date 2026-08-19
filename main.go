// Command tln-plugin is the plain, no-extensions build of the tln plugin: it
// serves the Tln runtime with no bundle plugins. To ship extra plugins (a
// solver, extra connectors, …) an EXTERNAL bundle imports package runtime and
// calls runtime.Serve(plugins...) with the plugins its own mod.tln declares —
// this repo never depends on any specific plugin.
package main

import (
	"log/slog"
	"os"

	"github.com/opentalon/tln-plugin/runtime"
)

func main() {
	if err := runtime.Serve(); err != nil {
		slog.Error("tln-plugin: serve", "error", err)
		os.Exit(1)
	}
}
