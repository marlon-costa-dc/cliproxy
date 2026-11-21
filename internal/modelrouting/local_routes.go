package modelrouting

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/ratelimit"
)

// LocalRoute is one consumer's dedicated route to a locally served runtime
// model. CLIProxy serves its name as a selectable alias whose candidates are
// the live registered routes of the runtime model, paced by its rate limit.
type LocalRoute struct {
	Name           string          `yaml:"name" json:"name"`
	RuntimeModelID string          `yaml:"runtime-model-id" json:"runtime-model-id"`
	RateLimit      *ratelimit.Spec `yaml:"rate-limit,omitempty" json:"rate-limit,omitempty"`
}

func (cfg *Config) validateLocalRoutes() error {
	names := make(map[string]struct{}, len(cfg.Aliases)+len(cfg.LocalRoutes))
	for _, alias := range cfg.Aliases {
		names[strings.ToLower(alias.Name)] = struct{}{}
	}
	for index, route := range cfg.LocalRoutes {
		path := fmt.Sprintf("model-routing.local-routes[%d]", index)
		if err := requireCanonical(path+".name", route.Name); err != nil {
			return err
		}
		if err := requireCanonical(path+".runtime-model-id", route.RuntimeModelID); err != nil {
			return err
		}
		key := strings.ToLower(route.Name)
		if _, duplicate := names[key]; duplicate {
			return fmt.Errorf("%s.name: duplicates another alias or local route", path)
		}
		names[key] = struct{}{}
		if _, err := route.RateLimit.Config(); err != nil {
			return fmt.Errorf("%s.rate-limit.%w", path, err)
		}
	}
	return nil
}
