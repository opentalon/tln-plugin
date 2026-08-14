package main

import (
	"log/slog"
	"os"

	"github.com/opentalon/opentalon/pkg/plugin"
)

func main() {
	if err := plugin.Serve(&handler{}); err != nil {
		slog.Error("tln-plugin: serve", "error", err)
		os.Exit(1)
	}
}
