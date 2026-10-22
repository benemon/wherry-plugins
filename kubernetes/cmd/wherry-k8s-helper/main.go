// Command wherry-k8s-helper runs inside wherry agent pods as their init container and outputs holder, and serves the
// plugin's execs; it is not run by hand.
package main

import (
	"os"
	"syscall"

	"github.com/benemon/wherry-plugins/kubernetes/helper"
)

func main() {
	os.Exit(helper.Run(helper.Env{Root: "/", Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Signal: syscall.Kill}, os.Args[1:]))
}
