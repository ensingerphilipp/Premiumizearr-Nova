package directory_watcher

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

var errWatcherNotInitialized = errors.New("watcher not initialized")

// NewWatchDirectory creates a new WatchDirectory.
func NewDirectoryWatcher(path string, recursive bool, matchFunction func(string) int, callbackFunction func(string)) *WatchDirectory {
	return &WatchDirectory{
		Path: path,
		// TODO (Unused): Add recursive abilities
		Recursive:        recursive,
		MatchFunction:    matchFunction,
		CallbackFunction: callbackFunction,
	}
}

func (w *WatchDirectory) Watch() error {
	var err error
	w.Watcher, err = fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	go func() {
		var action int = 1
		for {
			select {
			case event, ok := <-w.Watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Create == fsnotify.Create {
					action = w.MatchFunction(event.Name)
					if action == 1 {
						w.CallbackFunction(event.Name)
					} else if action == 2 {
						w.Watcher.Add(event.Name)
					}
				}
			case _, ok := <-w.Watcher.Errors:
				if !ok {
					return
				}
			}
		}
	}()

	cleanPath := filepath.Clean(w.Path)
	_, err = os.Stat(cleanPath)
	if os.IsNotExist(err) {
		return err
	}

	err = w.Watcher.Add(cleanPath)
	if err != nil {
		return err
	}

	return nil
}

// AddWatchPath watches an additional directory on top of the root Path
func (w *WatchDirectory) AddWatchPath(path string) error {
	if w == nil || w.Watcher == nil {
		return errWatcherNotInitialized
	}
	return w.Watcher.Add(filepath.Clean(path))
}

// RemoveWatchPath stops watching a directory previously added via AddWatchPath
func (w *WatchDirectory) RemoveWatchPath(path string) error {
	if w == nil || w.Watcher == nil {
		return errWatcherNotInitialized
	}
	return w.Watcher.Remove(filepath.Clean(path))
}

// UpdatePath atomically points the watcher at a new directory: the
// Remove of the old path, the Path update, and the Add of the new path run
// under the internal lock so two concurrent UpdatePath calls cannot
// interleave (the watcher would be left with the wrong path or a
// half-removed watch).
func (w *WatchDirectory) UpdatePath(path string) error {
	if w == nil || w.Watcher == nil {
		return errWatcherNotInitialized
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Watcher.Remove(w.Path)
	w.Path = path
	return w.Watcher.Add(w.Path)
}

func (w *WatchDirectory) Stop() error {
	if w == nil || w.Watcher == nil {
		return nil
	}
	return w.Watcher.Close()
}
