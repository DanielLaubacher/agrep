// Package watch implements --watch mode with raw inotify + epoll: it
// tails watched files, reads only newly appended content via pread from
// the last offset, and handles truncation (log rotation) by resetting.
package watch

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Event represents a file change event.
type Event struct {
	Path string
	Type EventType
	Err  error
}

// EventType identifies the kind of file change.
type EventType int

const (
	EventModified EventType = iota
	EventCreated
	EventDeleted
)

// Watcher watches files and directories for changes using raw inotify + epoll.
type Watcher struct {
	inotifyFd int
	epollFd   int
	watches   map[int]string   // wd -> path
	offsets   map[string]int64 // path -> last read offset
	done      chan struct{}
}

// New creates a new inotify-based file watcher.
func New() (*Watcher, error) {
	ifd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("inotify_init1: %w", err)
	}

	efd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		unix.Close(ifd)
		return nil, fmt.Errorf("epoll_create1: %w", err)
	}

	// Register inotify fd with epoll
	event := unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(ifd),
	}
	if err := unix.EpollCtl(efd, unix.EPOLL_CTL_ADD, ifd, &event); err != nil {
		unix.Close(efd)
		unix.Close(ifd)
		return nil, fmt.Errorf("epoll_ctl: %w", err)
	}

	return &Watcher{
		inotifyFd: ifd,
		epollFd:   efd,
		watches:   make(map[int]string),
		offsets:   make(map[string]int64),
		done:      make(chan struct{}),
	}, nil
}

// Add adds a path to watch. For directories, watches for new/modified files.
// For files, watches for modifications and moves (log rotation).
func (w *Watcher) Add(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	mask := uint32(unix.IN_MODIFY | unix.IN_CREATE | unix.IN_MOVED_TO | unix.IN_MOVE_SELF | unix.IN_DELETE_SELF)

	wd, err := unix.InotifyAddWatch(w.inotifyFd, absPath, mask)
	if err != nil {
		return fmt.Errorf("inotify_add_watch %s: %w", absPath, err)
	}

	w.watches[wd] = absPath

	// Initialize offset for files
	info, err := os.Stat(absPath)
	if err == nil && !info.IsDir() {
		w.offsets[absPath] = info.Size()
	}

	return nil
}

// Events returns a channel of file events. Blocks until Close() is called.
func (w *Watcher) Events() <-chan Event {
	ch := make(chan Event, 64)
	go func() {
		defer close(ch)
		buf := make([]byte, 4096)
		events := make([]unix.EpollEvent, 1)

		for {
			select {
			case <-w.done:
				return
			default:
			}

			// Wait for events with 100ms timeout
			n, err := unix.EpollWait(w.epollFd, events, 100)
			if err != nil {
				if err == unix.EINTR {
					continue
				}
				ch <- Event{Err: fmt.Errorf("epoll_wait: %w", err)}
				return
			}
			if n == 0 {
				continue
			}

			// Read inotify events
			nbytes, err := unix.Read(w.inotifyFd, buf)
			if err != nil {
				if err == unix.EAGAIN {
					continue
				}
				ch <- Event{Err: fmt.Errorf("read inotify: %w", err)}
				return
			}

			// Parse inotify events from buffer
			w.parseEvents(buf[:nbytes], ch)
		}
	}()
	return ch
}

// inotify event header layout:
//   int32  wd       (offset 0)
//   uint32 mask     (offset 4)
//   uint32 cookie   (offset 8)
//   uint32 len      (offset 12)
//   char   name[]   (offset 16)
const inotifyEventSize = 16

func (w *Watcher) parseEvents(buf []byte, ch chan<- Event) {
	offset := 0
	for offset+inotifyEventSize <= len(buf) {
		wd := int32(binary.LittleEndian.Uint32(buf[offset:]))
		mask := binary.LittleEndian.Uint32(buf[offset+4:])
		// cookie at offset+8 (unused)
		nameLen := int(binary.LittleEndian.Uint32(buf[offset+12:]))

		var name string
		if nameLen > 0 {
			nameStart := offset + inotifyEventSize
			nameEnd := nameStart + nameLen
			if nameEnd > len(buf) {
				break
			}
			nameBytes := buf[nameStart:nameEnd]
			// Trim NUL padding
			for i, b := range nameBytes {
				if b == 0 {
					nameBytes = nameBytes[:i]
					break
				}
			}
			name = string(nameBytes)
		}

		offset += inotifyEventSize + nameLen

		dirPath := w.watches[int(wd)]
		var path string
		if name != "" {
			path = filepath.Join(dirPath, name)
		} else {
			path = dirPath
		}

		switch {
		case mask&unix.IN_CREATE != 0 || mask&unix.IN_MOVED_TO != 0:
			ch <- Event{Path: path, Type: EventCreated}
		case mask&unix.IN_MODIFY != 0:
			ch <- Event{Path: path, Type: EventModified}
		case mask&unix.IN_DELETE_SELF != 0 || mask&unix.IN_MOVE_SELF != 0:
			ch <- Event{Path: path, Type: EventDeleted}
		}
	}
}

// ReadNew reads new content appended to a file since the last read.
// Returns the new bytes and updates the tracked offset.
func (w *Watcher) ReadNew(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOATIME, 0)
	if err != nil {
		fd, err = unix.Open(path, unix.O_RDONLY, 0)
		if err != nil {
			return nil, err
		}
	}
	defer unix.Close(fd)

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}

	lastOffset := w.offsets[path]
	newSize := stat.Size

	if newSize <= lastOffset {
		// File was truncated or no new data
		if newSize < lastOffset {
			w.offsets[path] = 0
			lastOffset = 0
		} else {
			return nil, nil
		}
	}

	toRead := int(newSize - lastOffset)
	if toRead == 0 {
		return nil, nil
	}

	buf := make([]byte, toRead)
	n, err := unix.Pread(fd, buf, lastOffset)
	if err != nil {
		return nil, err
	}

	w.offsets[path] = lastOffset + int64(n)
	return buf[:n], nil
}

// Close stops the watcher and releases resources.
func (w *Watcher) Close() error {
	close(w.done)
	unix.Close(w.epollFd)
	return unix.Close(w.inotifyFd)
}
