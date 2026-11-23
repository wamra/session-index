package search

import "time"

// Tuning returns the knobs SetTuning sets, in the same order.
func Tuning() (k, rrf int, embedWait time.Duration, terms int) {
	return defaultK, rrfK, embedTimeout, maxTerms
}
