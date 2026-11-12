package stringqueue

import (
	"fmt"
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

func TestAddIfAbsentIgnoresDuplicatePath(t *testing.T) {
	queue := NewStringQueue()

	queue.AddIfAbsent("/blackhole/request.torrent")
	queue.AddIfAbsent("/blackhole/request.torrent")

	if got := queue.Len(); got != 1 {
		t.Fatalf("queue length = %d, want 1", got)
	}
}

func TestAddIfAbsentIgnoresConcurrentDuplicatePaths(t *testing.T) {
	queue := NewStringQueue()
	const additions = 100

	var waitGroup sync.WaitGroup
	waitGroup.Add(additions)
	for range additions {
		go func() {
			defer waitGroup.Done()
			queue.AddIfAbsent("/blackhole/request.torrent")
		}()
	}
	waitGroup.Wait()

	if got := queue.Len(); got != 1 {
		t.Fatalf("queue length = %d, want 1", got)
	}
}

func TestAddIfAbsentAllowsPathAfterPop(t *testing.T) {
	queue := NewStringQueue()
	const filePath = "/blackhole/request.torrent"
	queue.AddIfAbsent(filePath)

	ok, poppedPath := queue.PopTopOfQueue()
	if !ok {
		t.Fatal("PopTopOfQueue() reported an empty queue")
	}
	if poppedPath != filePath {
		t.Fatalf("popped path = %q, want %q", poppedPath, filePath)
	}

	queue.AddIfAbsent(filePath)
	if got := queue.Len(); got != 1 {
		t.Fatalf("queue length after re-adding popped path = %d, want 1", got)
	}
}

func TestAddIfAbsentReportsInsertionAndPreservesOrder(t *testing.T) {
	queue := NewStringQueue()
	if !queue.AddIfAbsent("a") || !queue.AddIfAbsent("b") || queue.AddIfAbsent("a") {
		t.Fatal("incorrect insertion result")
	}
	for _, want := range []string{"a", "b"} {
		ok, got := queue.PopTopOfQueue()
		if !ok || got != want {
			t.Fatalf("popped %q, want %q", got, want)
		}
	}
	if !queue.AddIfAbsent("a") {
		t.Fatal("popped path cannot be requeued")
	}
}

func BenchmarkQueueRescan(b *testing.B) {
	for range b.N {
		queue := NewStringQueue()
		for i := range 10000 {
			queue.AddIfAbsent(fmt.Sprint(i))
		}
		for i := range 10000 {
			queue.AddIfAbsent(fmt.Sprint(i))
		}
	}
}
