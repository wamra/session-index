package ingest

import "time"

// Backoff returns the first retry wait.
func Backoff() time.Duration { return backoffBase }
