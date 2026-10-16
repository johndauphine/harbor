package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Release builds may set version with -ldflags '-X main.version=vX.Y.Z'.
var version = "development"

// Packaging sets these explicitly because Go does not detect linked Git worktrees.
// Ordinary builds retain the toolchain-provided VCS information below.
var releaseRevision string
var releaseModified string

func versionText() string {
	revision := "unknown"
	modified := false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				revision = setting.Value
			}
			if setting.Key == "vcs.modified" {
				modified = setting.Value == "true"
			}
		}
	}
	if releaseRevision != "" {
		revision = releaseRevision
		modified = releaseModified == "true"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified {
		revision += "-modified"
	}
	return fmt.Sprintf("Harbor %s (%s; %s; %s/%s)", version, revision, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
