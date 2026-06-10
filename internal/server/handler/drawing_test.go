package handler

import (
	"os"
	"testing"
	"time"
)

func TestTempStore_PutGet(t *testing.T) {
	s := &tempStore{entries: make(map[string]tempEntry)}

	f, err := os.CreateTemp("", "ts-test-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	key := s.put(f.Name())
	if key == "" {
		t.Fatal("put returned empty key")
	}

	path, ok := s.get(key)
	if !ok || path != f.Name() {
		t.Fatalf("get: want (%s, true), got (%s, %v)", f.Name(), path, ok)
	}

	// pop consumes the entry
	path2, ok2 := s.pop(key)
	if !ok2 || path2 != f.Name() {
		t.Fatalf("pop: want (%s, true), got (%s, %v)", f.Name(), path2, ok2)
	}

	// subsequent get returns false
	_, ok3 := s.get(key)
	if ok3 {
		t.Fatal("get after pop should return false")
	}
}

func TestTempStore_Expiry(t *testing.T) {
	s := &tempStore{entries: make(map[string]tempEntry)}

	key := randomHex()
	s.mu.Lock()
	s.entries[key] = tempEntry{path: "/tmp/fake-drawing-test", expires: time.Now().Add(-time.Second)}
	s.mu.Unlock()

	_, ok := s.get(key)
	if ok {
		t.Fatal("expired entry should not be returned by get")
	}
	_, ok2 := s.pop(key)
	if ok2 {
		t.Fatal("expired entry should not be returned by pop")
	}
}

func TestWinParentDir(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`D:\foo\bar\file.pdf`, `D:\foo\bar`},
		{`D:\foo\file.pdf`, `D:\foo`},
		{`D:/foo/bar/file.pdf`, `D:\foo\bar`}, // forward slashes normalised
		{`file.pdf`, ``},
	}
	for _, tt := range tests {
		got := winParentDir(tt.input)
		if got != tt.want {
			t.Errorf("winParentDir(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
