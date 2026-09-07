package stringqueue

import "sync"

func NewStringQueue() *StringQueue {
	return &StringQueue{queue: make([]string, 0), queued: make(map[string]struct{}), mutex: &sync.Mutex{}}
}

func (UploadQueue *StringQueue) Len() int {
	UploadQueue.mutex.Lock()
	defer UploadQueue.mutex.Unlock()
	return len(UploadQueue.queue)
}

// Add queues a path unless it is already waiting to be processed.
func (UploadQueue *StringQueue) Add(path string) {
	UploadQueue.AddIfAbsent(path)
}

// AddIfAbsent reports whether the path was added. Membership and insertion
// are checked under the same lock, including concurrent watcher/poller calls.
func (UploadQueue *StringQueue) AddIfAbsent(path string) bool {
	UploadQueue.mutex.Lock()
	defer UploadQueue.mutex.Unlock()
	if _, exists := UploadQueue.queued[path]; exists {
		return false
	}
	UploadQueue.queued[path] = struct{}{}
	UploadQueue.queue = append(UploadQueue.queue, path)
	return true
}

func (UploadQueue *StringQueue) PopTopOfQueue() (bool, string) {
	UploadQueue.mutex.Lock()
	defer UploadQueue.mutex.Unlock()
	if len(UploadQueue.queue) > 0 {
		rtn := UploadQueue.queue[0]
		delete(UploadQueue.queued, rtn)
		UploadQueue.queue = UploadQueue.queue[1:]
		return true, rtn
	}
	return false, ""
}

func (UploadQueue *StringQueue) GetQueue() []string {
	UploadQueue.mutex.Lock()
	defer UploadQueue.mutex.Unlock()
	return UploadQueue.queue
}
