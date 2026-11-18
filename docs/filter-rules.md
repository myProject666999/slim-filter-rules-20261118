# Artifact filtering rules

This document is the normative description of how SlimToolkit filters the files
it puts into the optimized image. It complements the implementation in
`pkg/util/filter` and the dependency completion in
`pkg/util/filter/depclosure`.

## Rule sources

| Source | Action | Notes |
| --- | --- | --- |
| `--exclude-pattern` | exclude | repeatable |
| `--include-path`, `--include-path-file` | include | repeatable |
| `--filter-rules-file` | include / exclude | one rule per line |
| `--include-exe`, `--include-bin`, `--include-dir-bins` | kept explicitly | the target is always kept; its dependencies are completed and filtered by the exclude rules |
| internal (`/opt/_slim`, var/run lock dirs) | exclude | always applied |

Rules from a file:

```
# comment
include /var/log/app/keep.log
exclude /var/log/**
/plain/pattern            # a bare line is an exclude (plain pattern lists keep working)
-a.negation.style.line    # include
+a.plus.style.line        # include
```

## Matching semantics

1. Paths and patterns are normalized with `path.Clean` and are slash-separated.
   Relative patterns resolve against the container root (`/`), never against
   the working directory of the Slim process. `Options.Base` can relocate them
   for library use.
2. A rule that matches a directory applies to everything below that directory.
3. Wildcards:
   - `*` — any run of characters except `/`
   - `**` — any run of characters including `/`; `/a/**/b` also matches `/a/b`
   - `?` — exactly one character except `/`
   - `[...]`, `[a-z]`, `[^...]`, `[!...]` — character sets/ranges and both
     negation spellings
   - `{a,b}` — alternation; `\x` escapes a metacharacter
4. Dot files are matched by wildcards. `/app/*` matches `/app/.env`.
5. A trailing `/` makes a rule directory-only: it does not match a regular file
   with the same name, but it still covers the directory's contents.
6. A trailing `/**` is a subtree rule anchored at the preceding directory. It
   covers the anchor directory itself, and works with wildcard anchors
   (`/opt/*/lib/**` covers `/opt/foo/lib`).
7. Duplicate rules collapse into one; their origins are merged in a
   deterministic order.

## Conflict resolution

`Decide` collects every matching rule and orders them:

1. more literal leading segments (segments without wildcards);
2. more literal characters;
3. fewer `**` segments;
4. more path segments;
5. exact (no wildcard) over glob;
6. a rule that matches the path itself over a rule that only matches an
   ancestor directory;
7. include over exclude;
8. directory-only over plain when the normalized patterns are identical;
9. lexicographically smaller normalized pattern (deterministic tie-break).

The winner determines the result. `Decision.Explanation` and
`Decision.Matches` expose the winner and the competing rules.

Consequences for the common configurations:

- `exclude /var/log/**` + `include /var/log/app/keep.log` keeps exactly
  `keep.log`.
- `include /app` + `exclude /app/testdata/**` keeps `/app` but removes the test
  data and docs (`/app/docs` works the same way).
- Rule order never changes the result.

## Pruning

An excluded directory may only be skipped without visiting its children when no
include rule can possibly match below it (`Decision.PruneDir`). This keeps
include exceptions working inside an excluded tree and keeps walks over very
large trees cheap when there are none.

## Dependency completion

For executables kept explicitly, the shared-library closure is computed:

- the explicit executable itself is always kept (it was requested directly);
  only its dependencies are subject to the filter;
- each dependency is filtered with the same rule set; an explicit exclude wins
  over completion, and the library is reported with a warning that the program
  may fail to run;
- missing libraries and resolver failures are reported and skipped; the rest of
  the closure continues;
- bare library names are searched in `LD_LIBRARY_PATH`, the depending object's
  directory, then the standard library directories;
- symlinks are kept and their targets followed, with cycle detection;
- resolution depth is bounded.

Rationale for letting an explicit exclude win: the user asked for that path to
be absent from the image, and silently re-adding it would violate the explicit
request. A warning makes the consequence visible instead of hiding it.

## Complexity

Rules are precompiled once: `O(R * L)` time and `O(R)` memory for `R` rules of
total length `L`. Literal rules are indexed by exact path, literal subtree
anchors by directory; glob rules are bucketed by their first literal segment.
For `P` paths and depth `D` the typical cost is `O(P * (D + B))` where `B` is
the number of glob rules sharing a first segment. The worst case with many
`**`-leading rules remains `O(P * R * D)`; the legacy mode is always
`O(P * R)` because every pattern is matched verbatim.

## Behavior changes relative to the pre-existing implementation

| # | Change | Old behavior | New behavior | Mitigation |
| --- | --- | --- | --- | --- |
| 1 | A rule matching a directory covers its contents | each pattern matched the full path only | children inherit the match | Default. Exclude-only configs that relied on `/dir/**` are unaffected; `/dir` now also excludes children |
| 2 | Includes can override excludes | any exclude won | the more specific rule wins, then include | `SLIM_FILTER_LEGACY_SEMANTICS=1` |
| 3 | Relative patterns resolve against `/` | matched the walked absolute path verbatim | resolved against `/` and cleaned | legacy switch |
| 4 | A trailing `/` is directory-only | trailing slash was part of the pattern (usually never matched) | directory-only rule | legacy switch |
| 5 | A trailing `/**` covers the anchor directory itself | `/dir/**` did not match `/dir` | it does | legacy switch |
| 6 | Dependency completion respects excludes | excludes were ignored for `ldd` results, so excluded libraries were silently re-added | an excluded library is skipped and a warning says the program may fail to run; the explicitly requested executable itself is still kept | `SLIM_FILTER_LEGACY_SEMANTICS=1` restores ignore-excludes |
| 7 | Malformed patterns are rejected at compile time | silently skipped mid-walk | reported once, rule skipped (`CompileLenient`) | — |
| 8 | Decisions are explained | debug messages only | `Decision.Explanation` with the winning rule | — |

Under `SLIM_FILTER_LEGACY_SEMANTICS=1`, patterns are matched verbatim
(no normalization, no ancestor propagation), any matching exclude wins, and
completion ignores excludes. Rule sets containing only exclude patterns produce
identical results in both modes for paths the patterns match directly; the only
difference is change #1 (previously undefined, now documented).

## Testing

- `pkg/util/filter/filter_test.go` — table-driven specification; every case
  states its expectation and its reason independently of the implementation.
- `pkg/util/filter/filter_test.go` also contains `BenchmarkDecide100Rules` and
  `BenchmarkDecide500Rules`, which support the complexity claims.
- `pkg/util/filter/property_test.go` — randomized rule sets check that rule
  order is irrelevant, that adding an include never shrinks the result, that
  adding an exclude only removes paths where that exclude is the documented
  winner, that more specific includes stay protected, and that the compiled
  rule set is order-independent.
- `pkg/util/filter/depclosure/depclosure_test.go` — closure tests use an
  injected in-memory filesystem and resolver; no real binary or dynamic linker
  is needed.
- `pkg/util/fsutil/copydir_test.go` — the directory walker honors include
  exceptions inside excluded directories and preserves the legacy whole-directory
  skip.
- `pkg/app/sensor/artifact/filter_integration_test.go` — the sensor's rule
  construction follows the documented precedence and the legacy switch.
