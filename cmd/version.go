package cmd

// version is the CLI version string. It defaults to "dev" for local builds
// and is overridden at release time via -ldflags:
//
//	go build -ldflags="-X github.com/tyler/podcast-migrate/cmd.version=v0.8.0" .
var version = "dev"
