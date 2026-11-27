package stringqueue

import "sync"

type StringQueue struct {
	queue    []string
	inFlight map[string]struct{}
	mutex    *sync.Mutex
	queued   map[string]struct{}
}
