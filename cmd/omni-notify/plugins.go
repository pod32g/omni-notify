package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"

	"github.com/pod32g/omni-notify/internal/plugin"
	"github.com/pod32g/omni-notify/internal/plugins/oncall"
	"gopkg.in/yaml.v3"
)

// pluginFactories are the plugins compiled into this binary.
var pluginFactories = map[string]plugin.Factory{
	"oncall": oncall.New,
}

// startPlugins starts every plugin whose config section has `enabled: true`,
// in name order, and returns their handlers and a stop function. A section for
// a plugin this binary doesn't have is an error, so a typo can't silently
// leave a plugin off.
func startPlugins(ctx context.Context, sections map[string]yaml.Node, host plugin.Host) (map[string]http.Handler, func(), error) {
	names := make([]string, 0, len(sections))
	for name := range sections {
		if _, ok := pluginFactories[name]; !ok {
			return nil, nil, fmt.Errorf("plugins: unknown plugin %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	handlers := map[string]http.Handler{}
	var started []plugin.Plugin
	stop := func() {
		for i := len(started) - 1; i >= 0; i-- {
			started[i].Stop()
		}
	}
	for _, name := range names {
		section := sections[name]
		var on struct {
			Enabled bool `yaml:"enabled"`
		}
		if err := section.Decode(&on); err != nil {
			stop()
			return nil, nil, fmt.Errorf("plugins.%s: %w", name, err)
		}
		if !on.Enabled {
			continue
		}
		p, err := pluginFactories[name](section)
		if err != nil {
			stop()
			return nil, nil, fmt.Errorf("plugins.%s: %w", name, err)
		}
		h := host
		h.Log = host.Log.With("plugin", name)
		if err := p.Start(ctx, h); err != nil {
			stop()
			return nil, nil, fmt.Errorf("plugins.%s: start: %w", name, err)
		}
		started = append(started, p)
		handlers[name] = p.Handler()
		slog.Info("plugin started", "plugin", name, "prefix", "/"+name+"/")
	}
	return handlers, stop, nil
}
