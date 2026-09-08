package stringqueue

import "sync"

type StringQueue struct {
	queue  []string
	queued map[string]struct{}
	mutex  *sync.Mutex
}
