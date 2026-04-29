package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSocketPath_FlagHasHighestPriority(t *testing.T) {
	got := resolveSocketPath("/explicit/flag.sock", "/should/be/ignored.sock", "/nonexistent", "net-dhcp.sock")
	if got != "/explicit/flag.sock" {
		t.Errorf("expected explicit flag value, got %q", got)
	}
}

func TestResolveSocketPath_EnvVarBeatsAutoDiscovery(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "abc123"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := resolveSocketPath("", "/env/path.sock", dir, "net-dhcp.sock")
	if got != "/env/path.sock" {
		t.Errorf("expected env var value to win over auto-discovery, got %q", got)
	}
}

func TestResolveSocketPath_AutoDiscoverSingleSubdir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "plugin-id-xyz"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "plugin-id-xyz", "net-dhcp.sock")
	got := resolveSocketPath("", "", dir, "net-dhcp.sock")
	if got != want {
		t.Errorf("auto-discovery: want %q, got %q", want, got)
	}
}

func TestResolveSocketPath_FallbackOnMultipleSubdirs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"plugin-a", "plugin-b"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	want := filepath.Join(dir, "net-dhcp.sock")
	got := resolveSocketPath("", "", dir, "net-dhcp.sock")
	if got != want {
		t.Errorf("fallback on multiple subdirs: want %q, got %q", want, got)
	}
}

func TestResolveSocketPath_FallbackOnEmptyPluginsDir(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "net-dhcp.sock")
	got := resolveSocketPath("", "", dir, "net-dhcp.sock")
	if got != want {
		t.Errorf("fallback on empty plugins dir: want %q, got %q", want, got)
	}
}

func TestResolveSocketPath_FallbackOnMissingPluginsDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	want := filepath.Join(missing, "net-dhcp.sock")
	got := resolveSocketPath("", "", missing, "net-dhcp.sock")
	if got != want {
		t.Errorf("fallback on missing plugins dir: want %q, got %q", want, got)
	}
}

func TestResolveSocketPath_IgnoresFilesInPluginsDir(t *testing.T) {
	dir := t.TempDir()
	// A regular file in the plugins dir must not be confused for a plugin subdir.
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "real-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "real-plugin", "net-dhcp.sock")
	got := resolveSocketPath("", "", dir, "net-dhcp.sock")
	if got != want {
		t.Errorf("auto-discovery should ignore non-dir entries: want %q, got %q", want, got)
	}
}
