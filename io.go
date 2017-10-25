package runner

// This file contains routines for performing file io

import (
	"os"

	"github.com/go-stack/stack"
	"github.com/karlmutch/errors"
)

func ReadLast(fn string, max uint32) (data string, err errors.Error) {
	file, errOs := os.Open(fn)
	if errOs != nil {
		return "", errors.Wrap(errOs, fn).With("stack", stack.Trace().TrimRuntime())
	}
	defer file.Close()

	fi, errOs := file.Stat()
	if errOs != nil {
		return "", errors.Wrap(errOs, fn).With("stack", stack.Trace().TrimRuntime())
	}

	buf := make([]byte, max)
	n, errOs := file.ReadAt(buf, fi.Size()-int64(len(buf)))
	if errOs != nil {
		return "", errors.Wrap(errOs, fn).With("stack", stack.Trace().TrimRuntime())
	}
	buf = buf[:n]
	return string(buf), nil
}