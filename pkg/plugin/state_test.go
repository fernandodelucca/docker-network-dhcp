package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

// withTempStateDir redirects state persistence to a t.TempDir for the duration
// of the test and restores the original on cleanup.
func withTempStateDir(t *testing.T) string {
	t.Helper()
	original := stateDir
	stateDir = t.TempDir()
	t.Cleanup(func() { stateDir = original })
	return stateDir
}

func TestLoadState_MissingFileReturnsEmpty(t *testing.T) {
	withTempStateDir(t)
	got, err := loadState()
	if err != nil {
		t.Fatalf("loadState on empty dir: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("loadState on empty dir returned %d entries, want 0", len(got))
	}
}

func TestLoadState_EmptyFileReturnsEmpty(t *testing.T) {
	dir := withTempStateDir(t)
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadState()
	if err != nil {
		t.Fatalf("unexpected error on empty file: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries, want 0 from empty file", len(got))
	}
}

func TestSaveAndLoadState_RoundTrip(t *testing.T) {
	withTempStateDir(t)
	in := map[string]endpointState{
		"ep-1": {
			NetworkID:  "net-1",
			EndpointID: "ep-1",
			SandboxKey: "/var/run/docker/netns/abc",
			Mode:       NetworkModeBridge,
			Bridge:     "br0",
			IPv6:       false,
			IfIndex:    7,
			Hostname:   "container-1",
		},
		"ep-2": {
			NetworkID:  "net-2",
			EndpointID: "ep-2",
			SandboxKey: "/var/run/docker/netns/def",
			Mode:       NetworkModeMacvlan,
			Bridge:     "eth0",
			IPv6:       true,
			IfIndex:    13,
		},
	}

	if err := saveState(in); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	got, err := loadState()
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if len(got) != len(in) {
		t.Fatalf("entry count: got %d, want %d", len(got), len(in))
	}
	for id, want := range in {
		gotEntry, ok := got[id]
		if !ok {
			t.Errorf("missing entry %q after round-trip", id)
			continue
		}
		// Schema version is filled in by saveState; compare everything else.
		want.SchemaVersion = currentSchemaVersion
		if gotEntry != want {
			t.Errorf("entry %q mismatch:\n got: %+v\nwant: %+v", id, gotEntry, want)
		}
	}
}

func TestSaveState_AtomicWritePermissions(t *testing.T) {
	dir := withTempStateDir(t)
	if err := saveState(map[string]endpointState{"ep-1": {EndpointID: "ep-1"}}); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, stateFile))
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	// 0o600 — state file contains sandbox paths that shouldn't be world-readable.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file permission = %o, want 0600", perm)
	}
}

func TestSaveState_NoLeftoverTempFiles(t *testing.T) {
	dir := withTempStateDir(t)
	if err := saveState(map[string]endpointState{"ep-1": {EndpointID: "ep-1"}}); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		// Atomic write uses os.CreateTemp which produces *.tmp; the rename should
		// remove the temp file. Anything left behind is a leak.
		name := e.Name()
		if name != stateFile && name != networksFile {
			t.Errorf("unexpected file in stateDir after save: %q", name)
		}
	}
}

func TestLoadState_OldSchemaVersionUpgradesToOne(t *testing.T) {
	dir := withTempStateDir(t)
	// SchemaVersion 0 = pre-versioning; loadState should treat as v1.
	raw := `[{"network_id":"n","endpoint_id":"ep","sandbox_key":"/sk","mode":"bridge","bridge":"br0"}]`
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadState()
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	entry, ok := got["ep"]
	if !ok {
		t.Fatal("missing entry after load")
	}
	if entry.SchemaVersion != currentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d (auto-upgrade from 0)", entry.SchemaVersion, currentSchemaVersion)
	}
}

func TestLoadState_CorruptFileReturnsError(t *testing.T) {
	dir := withTempStateDir(t)
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(); err == nil {
		t.Error("expected error on corrupt state file, got nil")
	}
}

func TestLoadNetworks_MissingFileReturnsEmpty(t *testing.T) {
	withTempStateDir(t)
	got, err := loadNetworks()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries, want 0", len(got))
	}
}

func TestSaveAndLoadNetworks_RoundTrip(t *testing.T) {
	withTempStateDir(t)
	in := map[string]networkState{
		"net-1": {NetworkID: "net-1", Bridge: "br0", Mode: NetworkModeBridge, IPv6: false},
		"net-2": {NetworkID: "net-2", Bridge: "eth0", Mode: NetworkModeMacvlan, IPv6: true},
	}

	if err := saveNetworks(in); err != nil {
		t.Fatalf("saveNetworks: %v", err)
	}
	got, err := loadNetworks()
	if err != nil {
		t.Fatalf("loadNetworks: %v", err)
	}
	if len(got) != len(in) {
		t.Fatalf("entry count: got %d, want %d", len(got), len(in))
	}
	for id, want := range in {
		gotEntry, ok := got[id]
		if !ok {
			t.Errorf("missing entry %q", id)
			continue
		}
		if gotEntry != want {
			t.Errorf("entry %q mismatch:\n got: %+v\nwant: %+v", id, gotEntry, want)
		}
	}
}

func TestSaveNetworks_StateDirCreatedIfMissing(t *testing.T) {
	original := stateDir
	stateDir = filepath.Join(t.TempDir(), "nested", "subdir")
	t.Cleanup(func() { stateDir = original })

	if err := saveNetworks(map[string]networkState{"n": {NetworkID: "n"}}); err != nil {
		t.Fatalf("saveNetworks should create dir: %v", err)
	}
	if _, err := os.Stat(stateDir); err != nil {
		t.Errorf("stateDir not created: %v", err)
	}
}
