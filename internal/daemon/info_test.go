package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteReadClear(t *testing.T) {
	dir := t.TempDir()

	if got, err := Read(dir); err != nil || got != nil {
		t.Fatalf("Read before any write = %v, %v; want nil, nil", got, err)
	}

	want := Info{Addr: "127.0.0.1:18420", PID: 4321, Version: "v1.2.3", StartedAt: time.Now().Round(time.Second)}
	if err := Write(dir, want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Read(dir)
	if err != nil || got == nil {
		t.Fatalf("Read: %v, %v", got, err)
	}
	if got.Addr != want.Addr || got.PID != want.PID || got.Version != want.Version {
		t.Errorf("Read = %+v, want %+v", *got, want)
	}

	if err := Clear(dir); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got, _ := Read(dir); got != nil {
		t.Errorf("info survived Clear: %+v", got)
	}
	// Clearing what is already gone is what a second shutdown does.
	if err := Clear(dir); err != nil {
		t.Errorf("Clear on a missing file: %v", err)
	}
}

// A daemon that was killed leaves the file behind. Callers find out it is
// stale by failing to reach the address, so reading it must not be an error.
func TestReadIgnoresAnEmptyAddr(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte(`{"pid":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Read(dir); err != nil || got != nil {
		t.Errorf("Read = %v, %v; want nil, nil for an entry with no address", got, err)
	}
}

func TestReadRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Error("want an error naming the unreadable file")
	}
}

// The running daemon's address is tried first, because `serve --addr` is
// exactly the case where the config file is not where it is listening.
func TestAddrsPrefersTheRunningDaemon(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Info{Addr: "127.0.0.1:18420"}); err != nil {
		t.Fatal(err)
	}

	got := Addrs(dir, "127.0.0.1:8420")
	want := []string{"127.0.0.1:18420", "127.0.0.1:8420"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Addrs = %v, want %v", got, want)
	}
}

func TestAddrsWithoutADaemon(t *testing.T) {
	got := Addrs(t.TempDir(), "127.0.0.1:8420")
	if len(got) != 1 || got[0] != "127.0.0.1:8420" {
		t.Errorf("Addrs = %v, want just the configured address", got)
	}
}

// Probing the same address twice is pointless work on every status call.
func TestAddrsDeduplicates(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Info{Addr: "127.0.0.1:8420"}); err != nil {
		t.Fatal(err)
	}
	if got := Addrs(dir, "127.0.0.1:8420"); len(got) != 1 {
		t.Errorf("Addrs = %v, want one address", got)
	}
}

func TestPath(t *testing.T) {
	if got, want := Path("/tmp/x"), filepath.Join("/tmp/x", "daemon.json"); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}
