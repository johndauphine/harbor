//go:build !desktop || !windows

package main

const desktopEnabled = false

func runDesktop(string, bool) error { return nil }
