package app

import (
	"os"
	"testing"
)

func TestInProcessRegistryTracksAliveAndDeadProcesses(t *testing.T) {
	registry := newInProcessRegistry()
	if registry.ProcessAlive("missing", 1) != nil {
		t.Fatal("missing process returned a liveness value")
	}
	registry.Register("", 1, os.Getpid())
	registry.Register("invalid-pid", 1, 0)
	if registry.ProcessAlive("", 1) != nil || registry.ProcessAlive("invalid-pid", 1) != nil {
		t.Fatal("invalid registration was stored")
	}

	registry.Register("alive", 2, os.Getpid())
	alive := registry.ProcessAlive("alive", 2)
	if alive == nil || !*alive {
		t.Fatal("current process was not reported as alive")
	}

	registry.Register("dead", 3, 1<<30)
	dead := registry.ProcessAlive("dead", 3)
	if dead == nil || *dead {
		t.Fatal("invalid process was not reported as dead")
	}

	var nilRegistry *inProcessRegistry
	nilRegistry.Register("ignored", 1, os.Getpid())
	if nilRegistry.ProcessAlive("ignored", 1) != nil {
		t.Fatal("nil registry returned a liveness value")
	}
}

func TestInProcessRegistryUpdatesAnExistingAttempt(t *testing.T) {
	registry := newInProcessRegistry()
	registry.Register("job", 1, 1<<30)
	registry.Register("job", 1, os.Getpid())
	alive := registry.ProcessAlive("job", 1)
	if alive == nil || !*alive {
		t.Fatal("existing attempt was not updated")
	}
}
