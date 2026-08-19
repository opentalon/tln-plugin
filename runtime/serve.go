package runtime

import (
	"github.com/opentalon/opentalon/pkg/plugin"
	"github.com/opentalon/tln-language/pkg/tln"
)

// Plugin is a tln extension an external bundle compiles in and hands to Serve.
// Name is what a workflow references (e.g. `connector "solver" via asp`);
// Factory is the plugin's tln.PluginFactory.
//
// The manifest (mod.tln) and the list of Plugins live OUTSIDE this repo: a
// bundle project imports the plugin packages and passes them here. tln-plugin
// itself depends on no specific plugin — adding one never changes this repo.
type Plugin struct {
	Name    string
	Factory tln.PluginFactory
}

// Serve runs the tln-plugin gRPC server with the given bundle plugins wired in.
// Called with no arguments it is the plain, no-extensions build (tln-plugin's
// own cmd/tln-plugin does exactly that); an external bundle passes the plugins
// declared in its mod.tln.
func Serve(plugins ...Plugin) error {
	h := &handler{}
	for _, p := range plugins {
		h.pluginOpts = append(h.pluginOpts, tln.WithPlugin(p.Name, p.Factory))
		if h.connectorNames == nil {
			h.connectorNames = map[string]bool{}
		}
		h.connectorNames[p.Name] = true
	}
	return plugin.Serve(h)
}
