package fsutil

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/slimtoolkit/slim/pkg/util/filter"
)

func mkTree(t *testing.T, root string, files []string) {
	t.Helper()

	for _, f := range files {
		full := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()

	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.Mode().IsRegular() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	sort.Strings(out)
	return out
}

func TestCopyDirFilteredHonorsIncludesUnderExcludedDir(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	mkTree(t, src, []string{
		"var/log/app/keep.log",
		"var/log/app/other.log",
		"var/log/old.log",
		"app/bin/server",
	})

	rs, err := filter.Compile([]filter.Rule{
		{Pattern: filepath.Join(src, "var", "log", "**"), Action: filter.Exclude},
		{Pattern: filepath.Join(src, "var", "log", "app", "keep.log"), Action: filter.Include},
	}, filter.Options{})
	if err != nil {
		t.Fatal(err)
	}

	var pruned []string
	err, errs := CopyDirFiltered(true, src, dst, true, true, rs, nil, nil,
		func(p string, d filter.Decision) { pruned = append(pruned, p) })
	if err != nil {
		t.Fatalf("CopyDirFiltered: %v", err)
	}

	if len(errs) > 0 {
		t.Fatalf("copy errors: %v", errs)
	}

	got := listFiles(t, dst)
	want := []string{
		filepath.Join("app", "bin", "server"),
		filepath.Join("var", "log", "app", "keep.log"),
	}
	if len(got) != len(want) {
		t.Fatalf("files=%v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("files=%v, want %v", got, want)
		}
	}

	// The excluded directory still contains an included file, so the walker
	// must not skip it wholesale.
	for _, p := range pruned {
		if p == filepath.Join(src, "var", "log") {
			t.Fatalf("must not prune a directory with reachable includes: %v", pruned)
		}
	}
}

func TestCopyDirLegacyExcludesWholeDir(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	mkTree(t, src, []string{
		"var/log/app/keep.log",
		"var/log/app/other.log",
		"app/bin/server",
	})

	// The legacy exclude list is matched verbatim against the walked path.
	legacyExcludes := []string{filepath.Join(src, "var", "log", "app")}

	err, errs := CopyDir(true, src, dst, true, true, legacyExcludes, nil, nil)
	if err != nil {
		t.Fatalf("CopyDir: %v", err)
	}

	if len(errs) > 0 {
		t.Fatalf("copy errors: %v", errs)
	}

	got := listFiles(t, dst)
	want := []string{filepath.Join("app", "bin", "server")}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("files=%v, want %v", got, want)
	}
}
