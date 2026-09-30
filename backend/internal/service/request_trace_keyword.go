package service

import (
	"strings"
	"unicode/utf8"
)

// ValidRequestTraceKeyword bounds metadata-only search and rejects SQL LIKE
// wildcard-only queries that would act like an unfiltered whole-table scan.
func ValidRequestTraceKeyword(keyword string) bool {
	if keyword != strings.TrimSpace(keyword) || !utf8.ValidString(keyword) || utf8.RuneCountInString(keyword) < 3 || utf8.RuneCountInString(keyword) > 128 {
		return false
	}
	literal := 0
	for _, r := range keyword {
		if r == 0 || r == '\r' || r == '\n' {
			return false
		}
		if r != '%' && r != '_' && r != '\\' && r != ' ' {
			literal++
		}
	}
	return literal >= 2
}
