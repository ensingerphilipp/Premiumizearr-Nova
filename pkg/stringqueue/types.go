package stringqueue

import "sync"

type StringQueue struct {
	queue  []string
	counts map[string]int
	mutex  *sync.Mutex
}
