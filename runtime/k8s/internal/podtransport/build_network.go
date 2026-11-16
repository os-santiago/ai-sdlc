package podtransport

import (
	"context"
	"net"
	"net/netip"
	"reflect"
	"sort"
	"strconv"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
)

// BuildNetworkTargets derives targets from current fixed API and registry
// objects. Caller-supplied blackhole addresses are never accepted as proof.
func BuildNetworkTargets(objects map[string]map[string]any) ([]buildworkload.DeniedTarget, error) {
	seen := map[buildworkload.DeniedTarget]bool{}
	add := func(host any, port any) bool {
		h, ok := host.(string)
		if !ok {
			return false
		}
		ip, e := netip.ParseAddr(h)
		if e != nil || !ip.Is4() || !ip.IsPrivate() || ip.String() != h {
			return false
		}
		number, ok := port.(float64)
		if !ok || number < 1 || number > 65535 || float64(int(number)) != number {
			return false
		}
		seen[buildworkload.DeniedTarget{Host: h, Port: int(number)}] = true
		return true
	}
	for _, prefix := range []string{"default/", "kind-registry/"} {
		name := "kubernetes"
		if prefix == "kind-registry/" {
			name = "registry-service"
		}
		service, ok := objects[prefix+"service/"+name]
		if !ok {
			return nil, ErrBuildTransport
		}
		spec, ok := service["spec"].(map[string]any)
		if !ok {
			return nil, ErrBuildTransport
		}
		ips, ok := spec["clusterIPs"].([]any)
		if !ok || len(ips) != 1 || ips[0] != spec["clusterIP"] {
			return nil, ErrBuildTransport
		}
		ports, ok := spec["ports"].([]any)
		if !ok || len(ports) == 0 || len(ports) > 4 {
			return nil, ErrBuildTransport
		}
		for _, raw := range ports {
			p, ok := raw.(map[string]any)
			if !ok || p["protocol"] != "TCP" || !add(ips[0], p["port"]) {
				return nil, ErrBuildTransport
			}
		}
		endpoints, ok := objects[prefix+"endpoints/"+name]
		if !ok {
			return nil, ErrBuildTransport
		}
		if m, ok := endpoints["metadata"].(map[string]any); ok {
			if annotations, ok := m["annotations"].(map[string]any); ok && annotations["endpoints.kubernetes.io/over-capacity"] != nil {
				return nil, ErrBuildTransport
			}
		}
		subsets, ok := endpoints["subsets"].([]any)
		if !ok || len(subsets) == 0 || len(subsets) > 4 {
			return nil, ErrBuildTransport
		}
		for _, raw := range subsets {
			subset, ok := raw.(map[string]any)
			if !ok {
				return nil, ErrBuildTransport
			}
			if notReady, ok := subset["notReadyAddresses"].([]any); ok && len(notReady) > 0 {
				return nil, ErrBuildTransport
			}
			addresses, ok := subset["addresses"].([]any)
			if !ok || len(addresses) == 0 || len(addresses) > 4 {
				return nil, ErrBuildTransport
			}
			ports, ok := subset["ports"].([]any)
			if !ok || len(ports) == 0 || len(ports) > 4 {
				return nil, ErrBuildTransport
			}
			for _, a := range addresses {
				address, ok := a.(map[string]any)
				if !ok {
					return nil, ErrBuildTransport
				}
				for _, raw := range ports {
					p, ok := raw.(map[string]any)
					if !ok || p["protocol"] != "TCP" || !add(address["ip"], p["port"]) {
						return nil, ErrBuildTransport
					}
				}
			}
		}
	}
	if len(seen) < 2 || len(seen) > 8 {
		return nil, ErrBuildTransport
	}
	result := []buildworkload.DeniedTarget{}
	for t := range seen {
		result = append(result, t)
	}
	sortTargets(result)
	return result, nil
}
func sortTargets(targets []buildworkload.DeniedTarget) {
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Host == targets[j].Host {
			return targets[i].Port < targets[j].Port
		}
		return targets[i].Host < targets[j].Host
	})
}
func checkTargetEquality(expected, actual []buildworkload.DeniedTarget) bool {
	expected = append([]buildworkload.DeniedTarget(nil), expected...)
	actual = append([]buildworkload.DeniedTarget(nil), actual...)
	sortTargets(expected)
	sortTargets(actual)
	return reflect.DeepEqual(expected, actual)
}

// positiveBuildTargets proves reachability from the trusted controller network.
// The source script separately requires timed-out connections before fetching.
// Neither observation claims continuous enforcement during node/CNI failure.
func positiveBuildTargets(ctx context.Context, targets []buildworkload.DeniedTarget) error {
	if len(targets) < 2 || len(targets) > 8 {
		return ErrBuildTransport
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, t := range targets {
		ip, e := netip.ParseAddr(t.Host)
		if e != nil || !ip.Is4() || !ip.IsPrivate() || t.Port < 1 || t.Port > 65535 {
			return ErrBuildTransport
		}
		connection, e := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp4", net.JoinHostPort(t.Host, strconv.Itoa(t.Port)))
		if e != nil {
			return ErrBuildTransport
		}
		if connection.Close() != nil {
			return ErrBuildTransport
		}
	}
	return nil
}
