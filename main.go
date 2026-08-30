package main

import (
	"os"

	"github.com/initialed85/maclet/pkg/maclet"
)

func main() {
	os.Exit(maclet.Main(os.Args[1:]))
}
