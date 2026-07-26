package config

import "regexp"

// compileRe anchors a label regexp the way Prometheus does, so `team: db.*`
// means the whole value, not a substring.
func compileRe(expr string) (*regexp.Regexp, error) {
	return regexp.Compile("^(?:" + expr + ")$")
}

// MustCompileLabelRe compiles an anchored label regexp. Callers must have run
// Validate first, which reports bad expressions.
func MustCompileLabelRe(expr string) *regexp.Regexp {
	re, err := compileRe(expr)
	if err != nil {
		panic(err)
	}
	return re
}
