//go:build !unix

package daemon

import "os/exec"

func detachProcess(cmd *exec.Cmd) {}
