//go:build !windows

package imagerelay

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockLease(file *os.File) error   { return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
func unlockLease(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }
