package directory_watcher

import (
	"sync"

	"github.com/fsnotify/fsnotify"
)

// WatchDirectory watches a directory for changes.
type WatchDirectory struct {
	// Path is the path to the directory to watch.
	Path string
	// mu serializes UpdatePath against concurrent UpdatePath calls so the
	// Remove/Set/Add sequence cannot interleave with another one.
	mu sync.Mutex
	// Filter is the filter to apply to the directory.
	Filter string
	// Recursive is true if the directory should be watched recursively.
	Recursive bool
	// MatchFunction is the function to use to match files.
	MatchFunction func(string) int
	// Callback is the function to call when a file is created that matches with MatchFunction.
	CallbackFunction func(string)
	// watcher is the fsnotify watcher.
	Watcher *fsnotify.Watcher
}
