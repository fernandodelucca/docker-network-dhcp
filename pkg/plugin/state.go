package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// stateDir is the directory where the plugin persists its endpoint state so
// it can rebuild the in-memory map after a restart. The plugin's config.json
// must bind-mount the host's /var/lib/net-dhcp here.
const stateDir = "/var/lib/net-dhcp"

const stateFile = "state.json"

// currentSchemaVersion is incremented whenever endpointState gains or loses
// fields. loadState uses it to detect old files and migrate gracefully.
const currentSchemaVersion = 1

// endpointState is the persistent record of one endpoint we are managing.
// Anything we need to rebuild a dhcpManager after a restart goes here. The
// IP itself is *not* stored — at restore time we read it directly from the
// container's netns so the file never goes stale relative to the kernel.
type endpointState struct {
	SchemaVersion int    `json:"schema_version"`
	NetworkID     string `json:"network_id"`
	EndpointID    string `json:"endpoint_id"`
	// SandboxKey is the path libnetwork passed in JoinRequest. It points at
	// the container's netns (e.g. /var/run/docker/netns/<id>) and we use it
	// directly with netns.GetFromPath at restore time, sidestepping any
	// dependency on Docker API availability.
	SandboxKey string `json:"sandbox_key"`
	Mode       string `json:"mode"`
	Bridge     string `json:"bridge"`
	IPv6       bool   `json:"ipv6"`
	IfIndex    int    `json:"if_index"`
	Hostname   string `json:"hostname"`
}

func statePath() string {
	return filepath.Join(stateDir, stateFile)
}

// loadState reads the persisted endpoint table from disk. A missing file is
// not an error (first run, or pre-persistence install).
func loadState() (map[string]endpointState, error) {
	out := map[string]endpointState{}
	data, err := os.ReadFile(statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("read state file: %w", err)
	}
	if len(data) == 0 {
		return out, nil
	}
	var entries []endpointState
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("decode state file: %w", err)
	}
	for _, e := range entries {
		// SchemaVersion == 0 means the entry was written before versioning was
		// added. Treat it as version 1 (same fields, just missing the field).
		if e.SchemaVersion == 0 {
			e.SchemaVersion = currentSchemaVersion
		}
		out[e.EndpointID] = e
	}
	return out, nil
}

// saveState writes the endpoint table to disk atomically (temp + rename).
// The snapshot should be consistent at the call site before passing.
func saveState(entries map[string]endpointState) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	list := make([]endpointState, 0, len(entries))
	for _, e := range entries {
		e.SchemaVersion = currentSchemaVersion
		list = append(list, e)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}

	tmp, err := os.CreateTemp(stateDir, "state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()

	// Restrict permissions: state file contains sandbox paths and endpoint IDs.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp state: %w", err)
	}

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state: %w", err)
	}

	if err := os.Rename(tmpName, statePath()); err != nil {
		return fmt.Errorf("rename state file: %w", err)
	}

	// Sync the parent directory so the directory entry is durable after rename.
	if dir, err := os.Open(stateDir); err == nil {
		_ = dir.Sync()
		dir.Close()
	}

	return nil
}
