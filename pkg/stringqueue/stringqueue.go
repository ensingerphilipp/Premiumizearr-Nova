package stringqueue

import "sync"

func NewStringQueue() *StringQueue {
	return &StringQueue{
		queue:    make([]string, 0),
		queued:   make(map[string]struct{}),
		inFlight: make(map[string]struct{}),
		mutex:    &sync.Mutex{},
	}
}

func (UploadQueue *StringQueue) Len() int {
	UploadQueue.mutex.Lock()
	defer UploadQueue.mutex.Unlock()
	return len(UploadQueue.queue)
}

// Add queues a path unless it is already waiting or being processed.
func (UploadQueue *StringQueue) Add(path string) {
	UploadQueue.AddIfAbsent(path)
}

// AddIfAbsent reports whether the path was added. Waiting and in-flight
// membership are checked under the same lock, including concurrent scans.
func (UploadQueue *StringQueue) AddIfAbsent(path string) bool {
	UploadQueue.mutex.Lock()
	defer UploadQueue.mutex.Unlock()
	if _, exists := UploadQueue.queued[path]; exists {
		return false
	}
	if _, processing := UploadQueue.inFlight[path]; processing {
		return false
	}
	UploadQueue.queued[path] = struct{}{}
	UploadQueue.queue = append(UploadQueue.queue, path)
	return true
}

// PopTopOfQueue removes the oldest waiting path and hands it to the
// caller. From this point the path is in-flight until Done(path) is
// called: while in-flight, Add and AddIfAbsent reject it and GetQueue
// does not include it. Callers must call Done when processing is
// finished, on every completion path including errors, or the path
// cannot be re-queued until the process restarts.
func (UploadQueue *StringQueue) PopTopOfQueue() (bool, string) {
	UploadQueue.mutex.Lock()
	defer UploadQueue.mutex.Unlock()
	if len(UploadQueue.queue) > 0 {
		rtn := UploadQueue.queue[0]
		delete(UploadQueue.queued, rtn)
		UploadQueue.queue = UploadQueue.queue[1:]
		UploadQueue.inFlight[rtn] = struct{}{}
		return true, rtn
	}
	return false, ""
}

// Done releases a path after the worker has finished processing it.
func (UploadQueue *StringQueue) Done(path string) {
	UploadQueue.mutex.Lock()
	defer UploadQueue.mutex.Unlock()
	delete(UploadQueue.inFlight, path)
}

func (UploadQueue *StringQueue) GetQueue() []string {
	UploadQueue.mutex.Lock()
	defer UploadQueue.mutex.Unlock()
	return append([]string(nil), UploadQueue.queue...)
}
