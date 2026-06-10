//go:build windows

package main

import "io"

// bestEffortWriter forwards writes to the underlying writer but never reports an
// error, so a failing/invalid console (e.g. a Windows service with no console,
// where os.Stdout writes error) cannot abort an io.MultiWriter and stop the
// file log from being written.
type bestEffortWriter struct{ w io.Writer }

func (b bestEffortWriter) Write(p []byte) (int, error) {
	_, _ = b.w.Write(p)
	return len(p), nil
}

// newLogWriter combines a best-effort console writer with the log file. The
// file is the source of truth and must always be written; the console is
// best-effort so its errors never block the file write.
func newLogWriter(console, file io.Writer) io.Writer {
	return io.MultiWriter(bestEffortWriter{console}, file)
}
