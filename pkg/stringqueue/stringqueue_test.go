package stringqueue

import (
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

func TestAddAllowsPathAfterPop(t *testing.T) {
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
	if got := queue.Len(); got != 1 {
		t.Fatalf("queue length after re-adding popped path = %d, want 1", got)
	}
}
