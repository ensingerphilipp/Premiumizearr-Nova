package stringqueue

import (
	"sync"
	"testing"
)

func TestAddIfAbsentConcurrent(t *testing.T) {
	queue := NewStringQueue()
	var workers sync.WaitGroup
	for range 100 {
		workers.Add(1)
		go func() { defer workers.Done(); queue.AddIfAbsent("request.magnet") }()
	}
	workers.Wait()
	if queue.Len() != 1 {
		t.Fatalf("queue length = %d", queue.Len())
	}
	queue.PopTopOfQueue()
	if !queue.AddIfAbsent("request.magnet") {
		t.Fatal("popped path cannot be requeued")
	}
}

func TestAddPreservesAppendSemantics(t *testing.T) {
	queue := NewStringQueue()
	queue.Add("a")
	queue.Add("a")
	queue.Add("b")
	for _, want := range []string{"a", "a", "b"} {
		if queue.AddIfAbsent(want) {
			t.Fatal("duplicate insertion accepted")
		}
		ok, got := queue.PopTopOfQueue()
		if !ok || got != want {
			t.Fatalf("popped %q, want %q", got, want)
		}
	}
	if !queue.AddIfAbsent("a") {
		t.Fatal("membership remained after final duplicate popped")
	}
}

func TestGetQueueSnapshotDoesNotAlterMembership(t *testing.T) {
	queue := NewStringQueue()
	queue.Add("a")
	snapshot := queue.GetQueue()
	snapshot[0] = "b"
	if queue.AddIfAbsent("a") {
		t.Fatal("snapshot mutation invalidated membership")
	}
	_, got := queue.PopTopOfQueue()
	if got != "a" {
		t.Fatalf("snapshot changed queued path to %q", got)
	}
}
