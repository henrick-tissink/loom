//go:build !darwin

package main

// setDockBadge is a no-op off macOS (loom ships on macOS; this keeps
// `go build ./...` green on other platforms).
func setDockBadge(count int) {}
