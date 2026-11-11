package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

// wipeData deletes Wisp's databases and media cache from dir; wisp.log stays. Missing entries are fine.
func wipeData(dir string) error {
	var errs []error
	for _, n := range []string{"wisp.db", "wisp.db-wal", "wisp.db-shm", "session.db", "session.db-wal", "session.db-shm", "cache"} {
		errs = append(errs, os.RemoveAll(filepath.Join(dir, n)))
	}
	return errors.Join(errs...)
}

// relaunch starts a new Wisp process.
func relaunch() error {
	path, err := os.Executable()
	if err != nil {
		return err
	}
	return exec.Command(path).Start()
}
