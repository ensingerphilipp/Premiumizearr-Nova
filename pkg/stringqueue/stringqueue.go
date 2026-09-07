package stringqueue

import "sync"

func NewStringQueue() *StringQueue {
	return &StringQueue{queue: make([]string, 0), counts: make(map[string]int), mutex: &sync.Mutex{}}
}

func (UploadQueue *StringQueue) Len() int {
	UploadQueue.mutex.Lock()
	defer UploadQueue.mutex.Unlock()
	return len(UploadQueue.queue)
}

func (UploadQueue *StringQueue) Add(path string) {
	UploadQueue.mutex.Lock()
	defer UploadQueue.mutex.Unlock()
	UploadQueue.queue = append(UploadQueue.queue, path)
	UploadQueue.counts[path]++
}

// AddIfAbsent atomically queues a path only when it is not already waiting.
// Add retains its existing append semantics for public-module callers.
func (UploadQueue *StringQueue) AddIfAbsent(path string) bool {
	UploadQueue.mutex.Lock()
	defer UploadQueue.mutex.Unlock()
	if UploadQueue.counts[path] > 0 {
		return false
	}
	UploadQueue.queue = append(UploadQueue.queue, path)
	UploadQueue.counts[path]++
	return true
}

func (UploadQueue *StringQueue) PopTopOfQueue() (bool, string) {
	UploadQueue.mutex.Lock()
	defer UploadQueue.mutex.Unlock()
	if len(UploadQueue.queue) > 0 {
		rtn := UploadQueue.queue[0]
		UploadQueue.counts[rtn]--
		if UploadQueue.counts[rtn] == 0 {
			delete(UploadQueue.counts, rtn)
		}
		UploadQueue.queue = UploadQueue.queue[1:]
		return true, rtn
	}
	return false, ""
}

// GetQueue returns a snapshot; callers cannot mutate queue membership.
func (UploadQueue *StringQueue) GetQueue() []string {
	UploadQueue.mutex.Lock()
	defer UploadQueue.mutex.Unlock()
	return append([]string(nil), UploadQueue.queue...)
}
