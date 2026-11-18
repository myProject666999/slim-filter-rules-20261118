package filter

import "os"

// osGetenv is indirected so tests can exercise the environment switch without
// mutating the process environment.
var osGetenv = os.Getenv

func legacyModeFromEnv(getenv func(string) string) bool {
	switch getenv(EnvLegacySemantics) {
	case "", "0", "false", "False", "FALSE", "no", "off":
		return false
	default:
		return true
	}
}
