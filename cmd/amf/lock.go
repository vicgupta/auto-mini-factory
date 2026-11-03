package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The repo lock, .amf/watch.lock, keeps two amf processes (watch, run or
// auto) from working in one checkout at once, which would race on the same
// worktrees, branches and labels. It holds the owner's PID, host and start
// time; the owner touches it every lockHeartbeat. A lock is stale, and the
// next amf takes it over, when its PID is dead on this host or it hasn't
// been touched for lockStaleAfter (an amf on another host that died, or a
// reused PID).
var (
	lockHeartbeat  = time.Minute
	lockStaleAfter = 10 * time.Minute
)

// lockInfo is what .amf/watch.lock says.
type lockInfo struct {
	PID     int       `json:"pid"`
	Host    string    `json:"host"`
	Started time.Time `json:"started"`
	Command string    `json:"command"`
}

// repoLock is a lock this process holds.
type repoLock struct {
	path string
	stop chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

// lockPath is the lock file of the checkout at root.
func lockPath(root string) string { return filepath.Join(root, ".amf", "watch.lock") }

// acquireLock takes the repo lock of the checkout at root for command, or
// says which amf holds it. The file appears whole or not at all: amf
// writes it under a temporary name and hard-links it into place, which
// fails if it exists.
func acquireLock(root, command string) (*repoLock, error) {
	path := lockPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	data, _ := json.Marshal(lockInfo{PID: os.Getpid(), Host: host, Started: time.Now().UTC(), Command: command})
	tmp, err := os.CreateTemp(filepath.Dir(path), "watch.lock.*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(append(data, '\n'))
	if cerr := tmp.Close(); werr != nil || cerr != nil {
		return nil, errors.Join(werr, cerr)
	}
	for attempt := 0; ; attempt++ {
		err := os.Link(tmp.Name(), path)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			// No hard links here (some file systems): fall back to an
			// exclusive create.
			f, cerr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if cerr == nil {
				_, werr := f.Write(append(data, '\n'))
				if err := errors.Join(werr, f.Close()); err != nil {
					os.Remove(path)
					return nil, err
				}
				break
			}
			if !errors.Is(cerr, fs.ErrExist) {
				return nil, cerr
			}
		}
		held, info, stale := readLock(path, host)
		if !held {
			continue // released meanwhile
		}
		if !stale || attempt > 0 {
			return nil, fmt.Errorf("another amf (%s, pid %d on %s, since %s) holds %s; stop it first, or delete the file if that amf is gone",
				cmpOr(info.Command, "unknown command"), info.PID, cmpOr(info.Host, "an unknown host"), info.Started.Local().Format(time.DateTime), path)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	l := &repoLock{path: path, stop: make(chan struct{})}
	l.wg.Add(1)
	go l.heartbeat()
	return l, nil
}

// readLock reads the lock at path: whether it exists, what it says, and
// whether it is stale (see lockStaleAfter). An unreadable lock is stale
// once it is old.
func readLock(path, host string) (held bool, info lockInfo, stale bool) {
	st, err := os.Stat(path)
	if err != nil {
		return false, info, false
	}
	old := time.Since(st.ModTime()) > lockStaleAfter
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &info) != nil {
		return true, info, old
	}
	if info.Host == host && info.PID > 0 && !pidAlive(info.PID) {
		return true, info, true
	}
	return true, info, old
}

// heartbeat touches the lock until release.
func (l *repoLock) heartbeat() {
	defer l.wg.Done()
	t := time.NewTicker(lockHeartbeat)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			now := time.Now()
			_ = os.Chtimes(l.path, now, now)
		}
	}
}

// release removes the lock if it is still this process's. Safe to call
// more than once.
func (l *repoLock) release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		close(l.stop)
		l.wg.Wait()
		host, _ := os.Hostname()
		if held, info, _ := readLock(l.path, host); held && info.PID == os.Getpid() && info.Host == host {
			os.Remove(l.path)
		}
	})
}

// lockRepo takes the repo lock for command; Close releases it.
func (r *Runner) lockRepo(command string) error {
	l, err := acquireLock(r.Root, command)
	if err != nil {
		return err
	}
	r.lock = l
	return nil
}

// releaseLock releases the repo lock, if this Runner holds it.
func (r *Runner) releaseLock() {
	r.lock.release()
	r.lock = nil
}
