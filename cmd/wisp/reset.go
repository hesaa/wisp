package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

// wipeData deletes Wisp's databases and media cache from dir; wisp.log stays. Missing entries are fine.
// The sessions go first: if they cannot, wisp.db (which holds the PIN) stays, so live sessions are
// never left without their lock.
func wipeData(dir string) error {
	remove := func(names ...string) error {
		var errs []error
		for _, n := range names {
			errs = append(errs, os.RemoveAll(filepath.Join(dir, n)))
		}
		return errors.Join(errs...)
	}
	if err := remove("session.db", "session.db-wal", "session.db-shm"); err != nil {
		return err
	}
	return remove("wisp.db", "wisp.db-wal", "wisp.db-shm", "cache")
}

// relaunch starts a new Wisp process.
func relaunch() error {
	path, err := os.Executable()
	if err != nil {
		return err
	}
	return exec.Command(path).Start()
}
