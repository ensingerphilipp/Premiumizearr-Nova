package stringqueue

import (
	"fmt"
	"sync"
	"testing"
)

func TestAddIgnoresDuplicatePath(t *testing.T) {
	queue := NewStringQueue()

	queue.Add("/blackhole/request.torrent")
	queue.Add("/blackhole/request.torrent")

	if got := queue.Len(); got != 1 {
		t.Fatalf("queue length = %d, want 1", got)
	}
}

func TestAddIgnoresConcurrentDuplicatePaths(t *testing.T) {
	queue := NewStringQueue()
	const additions = 100

	var waitGroup sync.WaitGroup
	waitGroup.Add(additions)
	for range additions {
		go func() {
			defer waitGroup.Done()
			queue.Add("/blackhole/request.torrent")
		}()
	}
	waitGroup.Wait()

	if got := queue.Len(); got != 1 {
		t.Fatalf("queue length = %d, want 1", got)
	}
}

func TestAddAllowsPathAfterDone(t *testing.T) {
	queue := NewStringQueue()
	const filePath = "/blackhole/request.torrent"
	queue.Add(filePath)

	ok, poppedPath := queue.PopTopOfQueue()
	if !ok {
		t.Fatal("PopTopOfQueue() reported an empty queue")
	}
	if poppedPath != filePath {
		t.Fatalf("popped path = %q, want %q", poppedPath, filePath)
	}

	queue.Add(filePath)
	if got := queue.Len(); got != 0 {
		t.Fatalf("queue length while processing = %d, want 0", got)
	}
	queue.Done(filePath)
	queue.Add(filePath)
	if got := queue.Len(); got != 1 {
		t.Fatalf("queue length after processing completed = %d, want 1", got)
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
	if queue.AddIfAbsent("a") {
		t.Fatal("path was requeued while being processed")
	}
	queue.Done("a")
	queue.Done("b")
	if !queue.AddIfAbsent("a") {
		t.Fatal("processed path cannot be requeued")
	}
}

func TestGetQueueReturnsCopy(t *testing.T) {
	queue := NewStringQueue()
	queue.Add("/blackhole/a.nzb")
	queue.Add("/blackhole/b.nzb")

	snapshot := queue.GetQueue()
	if len(snapshot) != 2 {
		t.Fatalf("queue length = %d, want 2", len(snapshot))
	}
	// R1-7: mutate the returned snapshot; the queue's state and pop order
	// must be unaffected.
	snapshot[0] = "mutated"
	snapshot = append(snapshot, "appended")

	if got := queue.Len(); got != 2 {
		t.Fatalf("queue length after mutating the snapshot = %d, want 2", got)
	}
	for _, want := range []string{"/blackhole/a.nzb", "/blackhole/b.nzb"} {
		ok, got := queue.PopTopOfQueue()
		if !ok || got != want {
			t.Fatalf("popped %q, want %q", got, want)
		}
	}
}

func TestAddIfAbsentExcludesInFlightPath(t *testing.T) {
	queue := NewStringQueue()
	if !queue.AddIfAbsent("movie.nzb") {
		t.Fatal("initial AddIfAbsent did not add the path")
	}
	if ok, path := queue.PopTopOfQueue(); !ok || path != "movie.nzb" {
		t.Fatalf("PopTopOfQueue() = (%t, %q), want (true, movie.nzb)", ok, path)
	}
	if queue.AddIfAbsent("movie.nzb") {
		t.Fatal("AddIfAbsent added a path while it was being processed")
	}
	if length := queue.Len(); length != 0 {
		t.Fatalf("queue length while processing = %d, want 0", length)
	}

	queue.Done("movie.nzb")
	if !queue.AddIfAbsent("movie.nzb") {
		t.Fatal("AddIfAbsent did not allow the path after processing completed")
	}
}

func BenchmarkQueueRescan(b *testing.B) {
	for range b.N {
		queue := NewStringQueue()
		for i := range 10000 {
			queue.Add(fmt.Sprint(i))
		}
		for i := range 10000 {
			queue.Add(fmt.Sprint(i))
		}
	}
}
