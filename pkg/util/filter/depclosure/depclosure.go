// Package depclosure computes the shared-library dependency closure for the
// executables that survived artifact filtering.
//
// The closure is resolved with an injectable resolver (the sensor uses ldd via
// the sodeps package), which makes the package testable without running a
// dynamic linker or building real binaries.
//
// # Rules
//
//   - Dependencies are resolved transitively; every dependency is filtered
//     with the same RuleSet as the rest of the artifacts, so an explicit
//     exclude wins over automatic dependency completion. A dependency that is
//     removed this way is reported in Result.Excluded together with a warning
//     that the target may not run.
//   - A dependency that cannot be found (missing file, failed resolver call,
//     broken symlink) is reported in Result.Missing and is skipped; it never
//     aborts the closure of the other dependencies.
//   - Library names without a slash are looked up in the library search path:
//     LD_LIBRARY_PATH first, then the directory of the depending object, then
//     the standard library directories. Search path entries are taken from the
//     injected SearchPaths value (defaulting to LD_LIBRARY_PATH).
//   - Symbolic links are kept as entries in the result and their targets are
//     resolved and walked as dependencies too. Link cycles are detected.
package depclosure

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/slimtoolkit/slim/pkg/util/filter"
)

// Resolver returns the direct dependencies of an executable or shared object.
// Dependencies may be absolute paths or bare library names.
type Resolver func(filePath string) ([]string, error)

// ExcludedDependency records a dependency removed by an explicit exclude rule.
type ExcludedDependency struct {
	// Path is the dependency path.
	Path string
	// NeededBy is the object that pulled the dependency in.
	NeededBy string
	// Pattern is the winning exclude rule pattern.
	Pattern string
}

const (
	// MaxDepth bounds transitive resolution. Real shared objects are shallow;
	// the limit protects against pathological or cyclic linker graphs.
	MaxDepth = 32
)

// Options configures a closure computation.
type Options struct {
	// Rules filters dependency paths. Required.
	Rules *filter.RuleSet
	// Resolve resolves direct dependencies. Required.
	Resolve Resolver
	// SearchPaths are directories used to resolve bare library names. When
	// nil, they are read from LD_LIBRARY_PATH plus the standard library
	// directories.
	SearchPaths []string
	// FilterRoots filters the explicit executables with Rules as well. The
	// default keeps them: they were requested explicitly by the user, and only
	// their dependencies are subject to the automatic completion policy.
	FilterRoots bool
	// TranslatePath maps a host path reported by Resolver to the path used by
	// rules. If nil, paths are used as-is.
	TranslatePath func(string) string
	// FileExists and Lstat are injectable for tests. They default to os.Stat
	// and os.Lstat.
	FileExists func(string) bool
	Lstat      func(string) (os.FileInfo, error)
	Readlink   func(string) (string, error)
}

// Result is the outcome of a closure computation.
type Result struct {
	// Paths is the sorted, de-duplicated list of dependency paths to keep.
	Paths []string
	// Excluded lists dependencies removed by explicit exclude rules.
	Excluded []ExcludedDependency
	// Missing lists dependency names or paths that could not be resolved.
	Missing []string
	// Warnings is a stable, human readable summary. It always includes one
	// warning per excluded dependency (target may not run) and per missing
	// dependency, but those never invalidate the rest of the closure.
	Warnings []string
}

// restoreOptions fills in defaults.
func (o *Options) prepare() error {
	if o.Rules == nil {
		return fmt.Errorf("depclosure: nil rules")
	}

	if o.Resolve == nil {
		return fmt.Errorf("depclosure: nil resolver")
	}

	if o.SearchPaths == nil {
		o.SearchPaths = DefaultSearchPaths(os.Getenv("LD_LIBRARY_PATH"))
	}

	if o.FileExists == nil {
		o.FileExists = func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		}
	}

	if o.Lstat == nil {
		o.Lstat = os.Lstat
	}

	if o.Readlink == nil {
		o.Readlink = os.Readlink
	}

	if o.TranslatePath == nil {
		o.TranslatePath = func(p string) string { return p }
	}

	return nil
}

// DefaultSearchPaths combines LD_LIBRARY_PATH with the standard library
// directories.
func DefaultSearchPaths(ldLibraryPath string) []string {
	seen := map[string]struct{}{}
	var paths []string
	add := func(p string) {
		if p == "" {
			return
		}

		if !strings.HasPrefix(p, "/") {
			return
		}

		p = path.Clean(p)
		if _, found := seen[p]; found {
			return
		}

		seen[p] = struct{}{}
		paths = append(paths, p)
	}

	for _, p := range strings.Split(ldLibraryPath, string(os.PathListSeparator)) {
		add(p)
	}

	for _, p := range []string{
		"/lib",
		"/lib64",
		"/usr/lib",
		"/usr/lib64",
		"/usr/local/lib",
		"/usr/local/lib64",
	} {
		add(p)
	}

	return paths
}

// Hints describes the objects whose closure is needed.
type Hints struct {
	// Executables are paths kept explicitly (include-bin, include-exe, ...).
	Executables []string
	// NeededBy lets the caller attribute dependencies to their parent object.
	NeededBy map[string]string
}

// Compute builds the dependency closure of the requested executables.
//
// The executables themselves are part of Result.Paths (they are the roots of
// the closure), followed by their dependencies.
func Compute(hints Hints, opts Options) Result {
	var result Result

	if err := opts.prepare(); err != nil {
		result.Warnings = append(result.Warnings, err.Error())
		return result
	}

	queue := make([]depNode, 0, len(hints.Executables))
	kept := map[string]struct{}{}
	visited := map[string]struct{}{}
	excludedSeen := map[string]struct{}{}
	missingSeen := map[string]struct{}{}

	neededBy := func(p string) string {
		if hints.NeededBy != nil {
			if parent, found := hints.NeededBy[p]; found && parent != "" {
				return parent
			}
		}

		return "target"
	}

	addQueue := func(parent string, deps []string) {
		for _, dep := range deps {
			queue = append(queue, depNode{path: dep, neededBy: parent, depth: 1})
		}
	}

	for _, exe := range hints.Executables {
		if exe == "" {
			continue
		}

		exePath := normalizeDependency(opts, exe, "")
		if exePath == "" {
			addMissing(&result, missingSeen, opts.TranslatePath(exe), neededBy(exe))
			continue
		}

		if opts.FilterRoots {
			if decision := opts.Rules.Decide(opts.TranslatePath(exePath), false); !decision.Keep {
				addExcluded(&result, excludedSeen, ExcludedDependency{
					Path:     opts.TranslatePath(exePath),
					NeededBy: neededBy(exe),
					Pattern:  patternOf(decision),
				})

				continue
			}
		}

		deps, err := opts.Resolve(exePath)
		if err != nil {
			addMissing(&result, missingSeen, exePath+" (dependency resolution failed: "+err.Error()+")", neededBy(exe))
			continue
		}

		kept[exePath] = struct{}{}
		addQueue(exePath, deps)
	}

	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]

		if node.depth > MaxDepth {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("dependency resolution depth limit (%d) reached at %q; not walking further", MaxDepth, node.path))
			continue
		}

		depPath := normalizeDependency(opts, node.path, node.neededBy)
		if depPath == "" {
			addMissing(&result, missingSeen, node.path, node.neededBy)
			continue
		}

		if _, found := visited[depPath]; found {
			continue
		}

		visited[depPath] = struct{}{}

		rulePath := opts.TranslatePath(depPath)
		decision := opts.Rules.Decide(rulePath, false)
		if !decision.Keep {
			addExcluded(&result, excludedSeen, ExcludedDependency{
				Path:     rulePath,
				NeededBy: node.neededBy,
				Pattern:  patternOf(decision),
			})

			continue
		}

		kept[depPath] = struct{}{}

		// Symlinks: keep the link and walk the target as well.
		info, err := opts.Lstat(depPath)
		if err != nil {
			addMissing(&result, missingSeen, depPath, node.neededBy)
			continue
		}

		if info.Mode()&os.ModeSymlink != 0 {
			linkRef, err := opts.Readlink(depPath)
			if err != nil {
				addMissing(&result, missingSeen, depPath+" (unreadable symlink)", node.neededBy)
				continue
			}

			target := linkRef
			if !filepath.IsAbs(linkRef) {
				target = filepath.Join(filepath.Dir(depPath), linkRef)
			}

			target = filepath.Clean(target)
			queue = append(queue, depNode{path: target, neededBy: depPath, depth: node.depth})
		}

		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			continue
		}

		deps, err := opts.Resolve(depPath)
		if err != nil {
			addMissing(&result, missingSeen, depPath+" (dependency resolution failed: "+err.Error()+")", node.neededBy)
			continue
		}

		for _, dep := range deps {
			queue = append(queue, depNode{path: dep, neededBy: depPath, depth: node.depth + 1})
		}
	}

	result.Paths = make([]string, 0, len(kept))
	for p := range kept {
		result.Paths = append(result.Paths, p)
	}

	sort.Strings(result.Paths)
	sort.Slice(result.Excluded, func(i, j int) bool {
		if result.Excluded[i].Path != result.Excluded[j].Path {
			return result.Excluded[i].Path < result.Excluded[j].Path
		}

		return result.Excluded[i].NeededBy < result.Excluded[j].NeededBy
	})
	sort.Strings(result.Missing)

	for _, ex := range result.Excluded {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"dependency %q (needed by %q) was excluded by rule %q; the program may fail to run",
			ex.Path, ex.NeededBy, ex.Pattern))
	}

	for _, m := range result.Missing {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"dependency %q could not be resolved; continuing with the remaining dependencies", m))
	}

	return result
}

type depNode struct {
	path     string
	neededBy string
	depth    int
}

func patternOf(d filter.Decision) string {
	if d.Matched != nil {
		return d.Matched.Pattern
	}

	return "(unmatched default)"
}

// normalizeDependency resolves a dependency name to an absolute path.
// Bare names are searched in the library path rooted at the depending
// object's directory, then in Options.SearchPaths. It returns "" when the
// dependency cannot be located.
func normalizeDependency(opts Options, dep, neededBy string) string {
	if dep == "" {
		return ""
	}

	if strings.HasPrefix(dep, "/") {
		dep = path.Clean(dep)
		if !opts.FileExists(dep) {
			return ""
		}

		return dep
	}

	var dirs []string
	if neededBy != "" && strings.HasPrefix(neededBy, "/") {
		dirs = append(dirs, path.Dir(neededBy))
	}

	dirs = append(dirs, opts.SearchPaths...)

	for _, dir := range dirs {
		candidate := path.Join(dir, dep)
		if opts.FileExists(candidate) {
			return candidate
		}
	}

	return ""
}

func addExcluded(result *Result, seen map[string]struct{}, ex ExcludedDependency) {
	key := ex.Path + "\x00" + ex.NeededBy
	if _, found := seen[key]; found {
		return
	}

	seen[key] = struct{}{}
	result.Excluded = append(result.Excluded, ex)
}

func addMissing(result *Result, seen map[string]struct{}, name, neededBy string) {
	if _, found := seen[name]; found {
		return
	}

	seen[name] = struct{}{}
	result.Missing = append(result.Missing, name)
}
