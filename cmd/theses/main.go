package main

import (
	"fmt"
	"os"
)

// Version is stamped at build time: -ldflags="-X main.Version=<sha>".
var Version = "dev"

func main() {
	fmt.Fprintf(os.Stderr, "theses %s\n", Version)
	os.Exit(1)
}
