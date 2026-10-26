package main

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive flock on path, creating it if needed. With wait
// false it reports ok=false instead of blocking when another process holds it.
func lockFile(path string, wait bool) (unlock func(), ok bool, err error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), how); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
	}, true, nil
}
