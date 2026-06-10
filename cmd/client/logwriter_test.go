//go:build windows

package main

import (
	"bytes"
	"errors"
	"testing"
)

// failingWriter simulates os.Stdout in a Windows service: every write errors.
type failingWriter struct{ n int }

func (f *failingWriter) Write(p []byte) (int, error) {
	f.n++
	return 0, errors.New("no console: stdout handle is invalid")
}

// TestLogWriterFileSurvivesFailingConsole reproduces the service-mode logging
// bug: when the console writer (os.Stdout) errors, the file must still receive
// every write. A naive io.MultiWriter(os.Stdout, file) would abort on the
// console error and never reach the file.
func TestLogWriterFileSurvivesFailingConsole(t *testing.T) {
	console := &failingWriter{}
	var file bytes.Buffer

	w := newLogWriter(console, &file)

	lines := []string{"line one\n", "line two\n", "line three\n"}
	for _, l := range lines {
		n, err := w.Write([]byte(l))
		if err != nil {
			t.Fatalf("Write returned error despite failing console: %v", err)
		}
		if n != len(l) {
			t.Fatalf("Write returned n=%d, want %d", n, len(l))
		}
	}

	want := "line one\nline two\nline three\n"
	if got := file.String(); got != want {
		t.Errorf("file got %q, want %q", got, want)
	}
	if console.n != len(lines) {
		t.Errorf("console attempted %d writes, want %d (should still try)", console.n, len(lines))
	}
}

// TestBestEffortWriterSwallowsError verifies the wrapper never propagates the
// underlying writer's error and always reports a full write.
func TestBestEffortWriterSwallowsError(t *testing.T) {
	b := bestEffortWriter{&failingWriter{}}
	p := []byte("hello")
	n, err := b.Write(p)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if n != len(p) {
		t.Fatalf("n = %d, want %d", n, len(p))
	}
}
