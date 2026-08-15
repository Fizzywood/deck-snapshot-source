package main

import (
	"os"
	"path/filepath"

	"github.com/Fizzywood/deck-snapshot/internal/browseropen"
	"github.com/Fizzywood/deck-snapshot/internal/cli"
)

var (
	version                = "dev"
	googleClientID         = ""
	googleClientCredential = ""
)

func main() {
	if name := filepath.Base(os.Args[0]); name == "xdg-open" || name == "xdg-open.exe" {
		os.Exit(browseropen.Run(os.Args[1:]))
	}
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr, cli.Dependencies{
		Version: version, GoogleClientID: googleClientID, GoogleClientCredential: googleClientCredential,
	}))
}
