package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBusDirIsOptional(t *testing.T) {
	home := t.TempDir()
	if got := busDir("", home); got != "" {
		t.Errorf("no bus on this machine: busDir = %q; want none, so rows are not ? for a missing bus", got)
	}
	bus := filepath.Join(home, ".crius", "bus")
	if err := os.MkdirAll(bus, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := busDir("", home); got != bus {
		t.Errorf("bus exists: busDir = %q; want %q", got, bus)
	}
	if got := busDir("/given", home); got != "/given" {
		t.Errorf("--bus given: busDir = %q; want /given", got)
	}
}
