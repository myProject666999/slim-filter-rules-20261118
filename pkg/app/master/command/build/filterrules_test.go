package build

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/slimtoolkit/slim/pkg/util/fsutil"
)

func TestParseFilterRulesFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "rules.conf")
	content := "# comment\ninclude /var/log/app/keep.log\nexclude /var/log/**\n/plain/pattern\n"

	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	excludes := map[string]*fsutil.AccessInfo{}
	includes, err := parseFilterRulesFile(file, excludes)
	if err != nil {
		t.Fatalf("parseFilterRulesFile: %v", err)
	}

	if _, found := excludes["/var/log/**"]; !found {
		t.Fatalf("exclude rule missing: %v", excludes)
	}

	if _, found := excludes["/plain/pattern"]; !found {
		t.Fatalf("bare line must be an exclude: %v", excludes)
	}

	if _, found := includes["/var/log/app/keep.log"]; !found {
		t.Fatalf("include rule missing: %v", includes)
	}

	if _, found := excludes["/var/log/app/keep.log"]; found {
		t.Fatalf("include rule must not be added to excludes: %v", excludes)
	}
}

func TestParseFilterRulesFileErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "rules.conf")
	if err := os.WriteFile(file, []byte("include\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := parseFilterRulesFile(file, map[string]*fsutil.AccessInfo{}); err == nil {
		t.Fatal("expected an error for a keyword without a pattern")
	}

	if _, err := parseFilterRulesFile(filepath.Join(dir, "missing.conf"), map[string]*fsutil.AccessInfo{}); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestParseFilterRulesFileEmptyPath(t *testing.T) {
	excludes := map[string]*fsutil.AccessInfo{}
	includes, err := parseFilterRulesFile("", excludes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(includes) != 0 || len(excludes) != 0 {
		t.Fatalf("no rules expected: %v / %v", includes, excludes)
	}
}
