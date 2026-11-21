package auth

import (
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelrouting"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// localRouteCandidates serves a published local route as a lane alias: one
// candidate per live registered route of its runtime model, in registry order,
// each named after the local route so execution evidence reports it.
func (m *Manager) localRouteCandidates(local modelrouting.LocalRoute) ([]RoutingCandidate, error) {
	seen := make(map[string]struct{})
	var candidates []RoutingCandidate
	for index, registered := range registry.GetGlobalRegistry().RegisteredRouteSnapshots() {
		if registered.RuntimeModelID != local.RuntimeModelID || len(registered.RoutingProtocols()) == 0 {
			continue
		}
		key, errKey := registered.RoutingKey(index)
		if errKey != nil {
			return nil, routingContractError("route_not_selectable", errKey.Error(), http.StatusServiceUnavailable)
		}
		selector := modelrouting.SelectorForRoute(key, registered.RuntimeModelID)
		if _, exists := seen[selector]; exists {
			continue
		}
		seen[selector] = struct{}{}
		candidate, matched, errCandidate := m.liveRoutingCandidate(selector, local.RuntimeModelID)
		if errCandidate != nil {
			return nil, errCandidate
		}
		if !matched {
			continue
		}
		candidate.Alias = local.Name
		candidate.bootstrap = false
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}
