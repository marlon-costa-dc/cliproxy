package registry

import (
	_ "embed"
	"encoding/json"
	"strings"

	log "github.com/sirupsen/logrus"
)

//go:embed opencode_routes.json
var embeddedOpenCodeRoutesJSON []byte

// opencodeRouteDoc mirrors opencode_routes.json.
type opencodeRouteDoc struct {
	Zen  map[string]string            `json:"zen"`
	Go   map[string]string            `json:"go"`
	Free map[string]map[string]string `json:"free"`
}

var openCodeFreeRoutes map[string]map[string]string

// OpenCodeFreeModel reports zero-priced executable routes from the gateway catalog.
func OpenCodeFreeModel(provider, model string) bool {
	gateway := ""
	switch provider {
	case "opencode":
		gateway = "zen"
	case "opencode-go":
		gateway = "go"
	default:
		return false
	}
	_, ok := openCodeFreeRoutes[gateway][model]
	return ok
}

func loadOpenCodeRoutes() {
	var doc opencodeRouteDoc
	if err := json.Unmarshal(embeddedOpenCodeRoutesJSON, &doc); err != nil {
		log.Warnf("registry: failed to parse embedded opencode_routes.json: %v", err)
		return
	}
	openCodeRoutes["zen"] = normalizeRouteMap(doc.Zen)
	openCodeRoutes["go"] = normalizeRouteMap(doc.Go)
	openCodeFreeRoutes = doc.Free
	for gateway, routes := range doc.Free {
		for model, protocol := range routes {
			openCodeRoutes[gateway][model] = protocol
		}
	}
	for id := range openCodeRoutes["zen"] {
		if openCodeRoutes["zen"][id] == "gemini" {
			openCodeGeminiIDs[id] = true
		}
	}
}

func normalizeRouteMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		id := strings.ToLower(strings.TrimSpace(k))
		if id == "" {
			continue
		}
		out[id] = strings.ToLower(strings.TrimSpace(v))
	}
	return out
}

func init() {
	loadOpenCodeRoutes()
}
