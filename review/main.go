package main

import (
	"os"

	"nicholas.sh/cmd/review/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
