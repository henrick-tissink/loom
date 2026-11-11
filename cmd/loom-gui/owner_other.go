//go:build !linux

package main

import "context"

func acquireGUI() (func(), bool, error) { return func() {}, false, nil }
func listenActivation(context.Context)  {}
