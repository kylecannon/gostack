package main

import (
	"flag"
	"fmt"
)

// version is set at link time with -ldflags "-X main.version=<version>".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
	}
}
