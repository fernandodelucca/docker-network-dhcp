package plugin

import (
	"testing"
	"time"
)

func TestIsDHCPPlugin(t *testing.T) {
	cases := []struct {
		name   string
		driver string
		want   bool
	}{
		{"current canonical name with tag", "docker-network-dhcp:latest", true},
		{"current name with registry prefix", "ghcr.io/fernandodelucca/docker-network-dhcp:v1.0", true},
		{"legacy short name", "docker-net-dhcp:latest", true},
		{"legacy short name with registry", "registry.example.com/foo/docker-net-dhcp:abc", true},
		{"unrelated driver", "bridge", false},
		{"unrelated driver with similar name", "dhcp:latest", false},
		{"empty string", "", false},
		{"name without tag", "docker-network-dhcp", false}, // regex requires `:tag` suffix
		{"substring match must not pass", "not-docker-net-dhcp-suffix:latest", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsDHCPPlugin(tc.driver); got != tc.want {
				t.Errorf("IsDHCPPlugin(%q) = %v, want %v", tc.driver, got, tc.want)
			}
		})
	}
}

func TestShortID(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"abcdef0123456789longer", "abcdef012345"},
		{"exactly12chr", "exactly12chr"},
		{"short", "short"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			if got := shortID(tc.input); got != tc.want {
				t.Errorf("shortID(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestDecodeOpts_AllFields(t *testing.T) {
	in := map[string]interface{}{
		"Bridge":           "br0",
		"Mode":             "macvlan",
		"IPv6":             true,
		"lease_timeout":    "30s",
		"ignore_conflicts": "true",
		"skip_routes":      false,
	}
	opts, err := decodeOpts(in)
	if err != nil {
		t.Fatalf("decodeOpts unexpected error: %v", err)
	}
	if opts.Bridge != "br0" {
		t.Errorf("Bridge = %q, want %q", opts.Bridge, "br0")
	}
	if opts.Mode != "macvlan" {
		t.Errorf("Mode = %q, want %q", opts.Mode, "macvlan")
	}
	if !opts.IPv6 {
		t.Errorf("IPv6 = false, want true")
	}
	if opts.LeaseTimeout != 30*time.Second {
		t.Errorf("LeaseTimeout = %v, want 30s", opts.LeaseTimeout)
	}
	if !opts.IgnoreConflicts {
		t.Errorf("IgnoreConflicts = false, want true (parsed from string 'true')")
	}
	if opts.SkipRoutes {
		t.Errorf("SkipRoutes = true, want false")
	}
}

func TestDecodeOpts_DefaultsAreZeroValues(t *testing.T) {
	opts, err := decodeOpts(map[string]interface{}{"Bridge": "br0"})
	if err != nil {
		t.Fatalf("decodeOpts unexpected error: %v", err)
	}
	if opts.Bridge != "br0" {
		t.Errorf("Bridge = %q, want %q", opts.Bridge, "br0")
	}
	if opts.Mode != "" {
		t.Errorf("Mode = %q, want empty (default to bridge via NetMode())", opts.Mode)
	}
	if opts.NetMode() != NetworkModeBridge {
		t.Errorf("NetMode() = %q, want %q", opts.NetMode(), NetworkModeBridge)
	}
}

func TestDecodeOpts_RejectsUnknownFields(t *testing.T) {
	// ErrorUnused: true means a typo'd option (e.g. "Bridg") should be rejected
	// rather than silently ignored. This catches user errors at network creation
	// instead of producing confusing "no IP" failures later.
	_, err := decodeOpts(map[string]interface{}{
		"Bridge":         "br0",
		"UnknownOption":  "value",
	})
	if err == nil {
		t.Error("expected error for unknown option, got nil")
	}
}

func TestNetMode_DefaultIsBridge(t *testing.T) {
	if (DHCPNetworkOptions{}).NetMode() != NetworkModeBridge {
		t.Errorf("zero-value NetMode() should be bridge")
	}
	if (DHCPNetworkOptions{Mode: "ipvlan"}).NetMode() != NetworkModeIPvlan {
		t.Errorf("explicit Mode should pass through")
	}
}

func TestVethPairNames_StableAndDistinct(t *testing.T) {
	endpointID := "abcdef0123456789xyz"
	host, ctr := vethPairNames(endpointID)
	if host == ctr {
		t.Error("host and container veth names must differ")
	}
	if len(host) > 15 {
		// Linux limits interface names to 15 chars (IFNAMSIZ - 1).
		t.Errorf("host veth name %q exceeds Linux IFNAMSIZ (15 chars)", host)
	}
	if len(ctr) > 15 {
		t.Errorf("container veth name %q exceeds Linux IFNAMSIZ (15 chars)", ctr)
	}
	// Stability: same input → same output (used to find peer after restart).
	host2, ctr2 := vethPairNames(endpointID)
	if host != host2 || ctr != ctr2 {
		t.Error("vethPairNames must be deterministic")
	}
}
