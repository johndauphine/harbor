package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

// The listener is already bound before this runs. Pass the generated URL as a
// single argument, never through a shell, so its private fragment stays intact.
func openBrowser(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command, args = "/usr/bin/open", []string{url}
	case "windows":
		command, args = "rundll32.exe", []string{"url.dll,FileProtocolHandler", url}
	case "linux":
		command, args = "xdg-open", []string{url}
	default:
		return fmt.Errorf("automatic browser opening unavailable")
	}
	return exec.CommandContext(ctx, command, args...).Run()
}
