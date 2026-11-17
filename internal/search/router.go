// Package search implements routed vector/FTS search over the archive.
package search

import (
	"regexp"
	"strings"
)

var identRe = regexp.MustCompile(`(/[\w.-]+){2,}|--[a-z]|[a-z0-9]\.[a-z0-9-]+\.[a-z]|[a-z]_[a-z]|[a-z][A-Z]|\b[A-Z][A-Z0-9_]{4,}\b`)

// LooksLikeIdentifier reports whether q is a short, identifier-shaped query
// (path, flag, dotted name, snake/camel case, CONSTANT) best served by
// trigram BM25 rather than embeddings.
func LooksLikeIdentifier(q string) bool {
	return len(strings.Fields(q)) <= 3 && identRe.MatchString(q)
}
