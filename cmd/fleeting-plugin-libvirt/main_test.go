package main

import (
	"os/signal"
	"syscall"
	"testing"
)

func TestIgnoreQuit(t *testing.T) {
	t.Cleanup(func() { signal.Reset(syscall.SIGQUIT) })
	ignoreQuit()
	if !signal.Ignored(syscall.SIGQUIT) {
		t.Fatal("SIGQUIT is not ignored")
	}
}
