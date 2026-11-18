//go:build !darwin

package main

// isDataless reports whether path is a dataless cloud placeholder. Only macOS
// marks such files in stat flags; elsewhere it is always false.
func isDataless(path string) bool { return false }
