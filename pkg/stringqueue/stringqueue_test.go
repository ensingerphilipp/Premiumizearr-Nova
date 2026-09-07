package stringqueue

import "testing"

func TestAddUnique(t *testing.T) {
	queue := NewStringQueue()
	if added := queue.AddUnique("movie.nzb"); !added {
		t.Fatal("first AddUnique call did not add the path")
	}
	if added := queue.AddUnique("movie.nzb"); added {
		t.Fatal("second AddUnique call added a duplicate path")
	}
	if length := queue.Len(); length != 1 {
		t.Fatalf("queue length = %d, want 1", length)
	}
}
