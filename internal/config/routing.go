package config

import "strings"

// NormalizeRoutingStrategy resolves supported built-in strategy names.
func NormalizeRoutingStrategy(strategy string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "", "round-robin", "roundrobin", "rr":
		return "round-robin", true
	case "weighted-round-robin", "weightedroundrobin", "wrr":
		return "weighted-round-robin", true
	case "fill-first", "fillfirst", "ff":
		return "fill-first", true
	case "soonest-reset-first":
		return "soonest-reset-first", true
	default:
		return "", false
	}
}
