// Package buildinfo carries identity that every other package needs and none
// should own: the release version and the platform string.
//
// It lives alone so that config, client and cmd can all read it without
// importing each other. In the Observatory binary these were globals in
// package main, which is why client.go could reach `version` and `platformLabel`
// directly — a coupling that only worked because everything was one package.
package buildinfo

import "runtime"

// Version is stamped at build time by goreleaser:
//
//	-ldflags "-X github.com/nonlinear-xyz/shale/internal/buildinfo.Version=v1.2.3"
//
// The default marks a locally built binary so a bug report can tell the two
// apart.
var Version = "dev"

// Name is the binary name, used in user-facing errors and the User-Agent.
const Name = "shale"

// PlatformLabel is the GOOS/GOARCH pair, e.g. "darwin/arm64". It appears in the
// User-Agent, the machine record, and `shale version`.
func PlatformLabel() string { return runtime.GOOS + "/" + runtime.GOARCH }

// UserAgent identifies this build to the hub. Keeping the version and platform
// in the agent string means a server-side error log can tell which build and
// which platform produced a malformed request.
func UserAgent() string { return Name + "/" + Version + " (" + PlatformLabel() + ")" }
