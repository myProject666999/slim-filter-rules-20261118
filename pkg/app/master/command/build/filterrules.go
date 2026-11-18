package build

import (
	"fmt"
	"os"

	"github.com/slimtoolkit/slim/pkg/util/filter"
	"github.com/slimtoolkit/slim/pkg/util/fsutil"
)

// parseFilterRulesFile loads a filter rules file and merges its exclude rules
// into excludes (in place). The include rules are returned so the caller can
// merge them into the include map.
//
// The file format is documented in pkg/util/filter (rulesfile.go). A line
// without an "include"/"exclude" keyword is treated as an exclude pattern,
// which makes a plain newline-delimited pattern list behave exactly like
// repeated --exclude-pattern flags.
func parseFilterRulesFile(filePath string, excludes map[string]*fsutil.AccessInfo) (map[string]*fsutil.AccessInfo, error) {
	includes := map[string]*fsutil.AccessInfo{}

	if filePath == "" {
		return includes, nil
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	rules, errs := filter.ParseRules(f, filePath)
	if len(errs) > 0 {
		return nil, fmt.Errorf("%d invalid rule line(s); first error: %w", len(errs), errs[0])
	}

	for _, rule := range rules {
		switch rule.Action {
		case filter.Include:
			includes[rule.Pattern] = nil
		default:
			excludes[rule.Pattern] = nil
		}
	}

	return includes, nil
}
