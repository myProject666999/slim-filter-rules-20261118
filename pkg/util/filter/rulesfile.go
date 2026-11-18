package filter

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Rule file format
//
// A rule file contains one rule per line:
//
//	# comment
//	include /app/data
//	exclude /app/testdata/**
//	-exclude-pattern-style-bare-pattern
//
// A line without a leading "include" or "exclude" keyword is treated as an
// exclude pattern, which keeps plain newline-delimited pattern lists (the
// format of the pre-existing --exclude-pattern style inputs) working with the
// same meaning. Blank lines and lines starting with '#' are ignored.
//
// A line that starts with '-' is a negated rule (an include) and a line that
// starts with '+' is an explicit include, mirroring the gitignore style that
// several path filtering tools use. The longer form ("include", "exclude") is
// preferred because it is unambiguous.

// ParseRules reads rules from r. baseSource is used to build rule origins for
// explanations (the file name).
func ParseRules(r io.Reader, baseSource string) ([]Rule, []error) {
	var rules []Rule

	var errs []error
	scanner := bufio.NewScanner(r)
	// Allow long patterns (path depth can be significant).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		action := Exclude
		pattern := line

		lower := strings.ToLower(line)
		switch {
		case lower == "include" || lower == "exclude":
			errs = append(errs, fmt.Errorf("%s:%d: %q keyword without a pattern", baseSource, lineNo, lower))
			continue
		case strings.HasPrefix(lower, "include "):
			action = Include
			pattern = strings.TrimSpace(line[len("include "):])
		case strings.HasPrefix(lower, "exclude "):
			action = Exclude
			pattern = strings.TrimSpace(line[len("exclude "):])
		case strings.HasPrefix(line, "-"), strings.HasPrefix(line, "+"):
			action = Include
			pattern = strings.TrimSpace(line[1:])
		}

		// Strip a trailing comment only when it is separated by whitespace so
		// patterns containing '#' stay usable.
		if idx := strings.Index(pattern, " #"); idx >= 0 {
			pattern = strings.TrimSpace(pattern[:idx])
		}

		if pattern == "" {
			errs = append(errs, fmt.Errorf("%s:%d: empty pattern", baseSource, lineNo))
			continue
		}

		rules = append(rules, Rule{
			Pattern: pattern,
			Action:  action,
			Source:  baseSource,
			Line:    lineNo,
		})
	}

	if err := scanner.Err(); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", baseSource, err))
	}

	return rules, errs
}

// ParseRulesString is a convenience wrapper around ParseRules.
func ParseRulesString(data, baseSource string) ([]Rule, []error) {
	return ParseRules(strings.NewReader(data), baseSource)
}
