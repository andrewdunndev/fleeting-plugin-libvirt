package main

import (
	"os/signal"
	"syscall"
	"testing"
)

func TestSIGQUITIgnoredAtStart(t *testing.T) {
	t.Cleanup(func() { signal.Reset(syscall.SIGQUIT) })
	if !signal.Ignored(syscall.SIGQUIT) {
		t.Fatal("SIGQUIT is not ignored")
	}
}
