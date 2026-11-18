//go:build linux
// +build linux

package artifact

import (
	"testing"

	"github.com/slimtoolkit/slim/pkg/ipc/command"
	"github.com/slimtoolkit/slim/pkg/util/filter"
	"github.com/slimtoolkit/slim/pkg/util/fsutil"
)

type fsutilAccessInfo = fsutil.AccessInfo

func testStore(excludes []string, includes map[string]*fsutilAccessInfo) *store {
	return &store{
		cmd: &command.StartMonitor{
			Excludes: excludes,
			Includes: includes,
		},
	}
}

func TestBuildArtifactRulesPrecedence(t *testing.T) {
	p := testStore(
		[]string{"/var/log/**", "/app/testdata/**"},
		map[string]*fsutilAccessInfo{
			"/var/log/app/keep.log": nil,
			"/app":                  nil,
		},
	)

	// The store's includes map is keyed by path; build it through the helper
	// path used by saveArtifacts.
	rs := p.buildArtifactRules(p.cmd.Excludes, nil)
	p.rules = rs

	cases := []struct {
		path  string
		isDir bool
		keep  bool
		why   string
	}{
		{"/var/log/app/keep.log", false, true, "specific include overrides the log dir exclude"},
		{"/var/log/syslog", false, false, "other log files stay excluded"},
		{"/app/bin/server", false, true, "app is included"},
		{"/app/testdata/x", false, false, "deeper testdata exclude wins inside the included app dir"},
	}

	for _, tc := range cases {
		if got := p.shouldKeepArtifact(tc.path, tc.isDir); got != tc.keep {
			t.Fatalf("%q: keep=%v, want %v (%s)", tc.path, got, tc.keep, tc.why)
		}
	}
}

func TestLegacyEnvKeepsExcludeWins(t *testing.T) {
	t.Setenv(filter.EnvLegacySemantics, "1")

	p := testStore(
		[]string{"/var/log/**"},
		map[string]*fsutilAccessInfo{"/var/log/app/keep.log": nil},
	)

	p.rules = p.buildArtifactRules(p.cmd.Excludes, nil)

	if p.shouldKeepArtifact("/var/log/app/keep.log", false) {
		t.Fatal("legacy semantics must let the exclude win")
	}
}
