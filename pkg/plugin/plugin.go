package plugin

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"

	dTypes "github.com/docker/docker/api/types"
	docker "github.com/docker/docker/client"
	"github.com/gorilla/handlers"
	"github.com/mitchellh/mapstructure"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	log "github.com/sirupsen/logrus"

	"github.com/fernandodelucca/Docker.Network.DHCP/pkg/util"
)

// DriverName is the name of the Docker Network Driver
const DriverName string = "net-dhcp"

// Network mode constants
const (
	NetworkModeBridge  = "bridge"
	NetworkModeMacvlan = "macvlan"
	NetworkModeIPvlan  = "ipvlan"
)

const defaultLeaseTimeout = 10 * time.Second

var driverRegexp = regexp.MustCompile(`(^|/)docker-net-dhcp:.+$`)

// IsDHCPPlugin checks if a Docker network driver is an instance of this plugin
func IsDHCPPlugin(driver string) bool {
	return driverRegexp.MatchString(driver)
}

// DHCPNetworkOptions contains options for the DHCP network driver
type DHCPNetworkOptions struct {
	Bridge          string
	Mode            string
	IPv6            bool
	LeaseTimeout    time.Duration `mapstructure:"lease_timeout"`
	IgnoreConflicts bool          `mapstructure:"ignore_conflicts"`
	SkipRoutes      bool          `mapstructure:"skip_routes"`
}

// NetMode returns the normalised network mode, defaulting to "bridge".
func (opts DHCPNetworkOptions) NetMode() string {
	if opts.Mode == "" {
		return NetworkModeBridge
	}
	return opts.Mode
}

func decodeOpts(input interface{}) (DHCPNetworkOptions, error) {
	var opts DHCPNetworkOptions
	optsDecoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           &opts,
		ErrorUnused:      true,
		WeaklyTypedInput: true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
		),
	})
	if err != nil {
		return opts, fmt.Errorf("failed to create options decoder: %w", err)
	}

	if err := optsDecoder.Decode(input); err != nil {
		return opts, err
	}

	return opts, nil
}

type joinHint struct {
	IPv4    *netlink.Addr
	IPv6    *netlink.Addr
	Gateway string
	// IfIndex is the interface index for macvlan/ipvlan endpoints, used to locate
	// the interface after Docker moves it into the container network namespace.
	IfIndex int
}

// Plugin is the DHCP network plugin
type Plugin struct {
	awaitTimeout time.Duration

	docker *docker.Client
	server http.Server

	mu             sync.Mutex
	joinHints      map[string]joinHint
	persistentDHCP map[string]*dhcpManager
}

// NewPlugin creates a new Plugin
func NewPlugin(awaitTimeout time.Duration) (*Plugin, error) {
	client, err := docker.NewClientWithOpts(
		docker.WithAPIVersionNegotiation(),
		docker.WithTimeout(2*time.Second),
		docker.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	p := Plugin{
		awaitTimeout: awaitTimeout,

		docker: client,

		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/NetworkDriver.GetCapabilities", p.apiGetCapabilities)

	mux.HandleFunc("/NetworkDriver.CreateNetwork", p.apiCreateNetwork)
	mux.HandleFunc("/NetworkDriver.DeleteNetwork", p.apiDeleteNetwork)

	mux.HandleFunc("/NetworkDriver.CreateEndpoint", p.apiCreateEndpoint)
	mux.HandleFunc("/NetworkDriver.EndpointOperInfo", p.apiEndpointOperInfo)
	mux.HandleFunc("/NetworkDriver.DeleteEndpoint", p.apiDeleteEndpoint)

	mux.HandleFunc("/NetworkDriver.Join", p.apiJoin)
	mux.HandleFunc("/NetworkDriver.Leave", p.apiLeave)

	p.server = http.Server{
		Handler: handlers.CustomLoggingHandler(nil, mux, util.WriteAccessLog),
	}

	return &p, nil
}

// Restore re-attaches persistent DHCP clients to containers that survived a
// plugin restart. It enumerates Docker networks driven by this plugin, locates
// each running container's interface inside its netns, reads the current IP(s)
// and starts a fresh udhcpc to keep the lease alive. Failures for individual
// endpoints are logged but do not abort the overall restore — partial recovery
// is preferable to refusing to start.
//
// The plugin starts before dockerd finishes "Loading containers", so the Docker
// API may not list containers yet. We retry Ping until the daemon responds (or
// the ctx expires) before issuing NetworkList.
func (p *Plugin) Restore(ctx context.Context) error {
	if err := util.AwaitCondition(ctx, func() (bool, error) {
		_, err := p.docker.Ping(ctx)
		if err != nil {
			log.WithError(err).Debug("Restore: waiting for Docker daemon")
			return false, nil
		}
		return true, nil
	}, time.Second); err != nil {
		return fmt.Errorf("docker daemon never became ready: %w", err)
	}

	nets, err := p.docker.NetworkList(ctx, dTypes.NetworkListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list networks: %w", err)
	}

	restored := 0
	for _, n := range nets {
		if !IsDHCPPlugin(n.Driver) {
			continue
		}

		opts, err := decodeOpts(n.Options)
		if err != nil {
			log.WithError(err).WithField("network", n.Name).Warn("Restore: failed to decode network options, skipping")
			continue
		}

		// NetworkInspect returns the per-endpoint container map keyed by container ID
		netInfo, err := p.docker.NetworkInspect(ctx, n.ID, dTypes.NetworkInspectOptions{})
		if err != nil {
			log.WithError(err).WithField("network", n.Name).Warn("Restore: failed to inspect network, skipping")
			continue
		}

		for ctrID, epInfo := range netInfo.Containers {
			if epInfo.EndpointID == "" {
				continue
			}
			if err := p.restoreEndpoint(ctx, n.ID, ctrID, epInfo, opts); err != nil {
				log.WithError(err).WithFields(log.Fields{
					"network":  n.ID[:12],
					"endpoint": epInfo.EndpointID[:12],
					"container": ctrID,
				}).Error("Restore: failed to restore endpoint")
				continue
			}
			restored++
		}
	}

	log.WithField("restored", restored).Info("Restore complete")
	return nil
}

// restoreEndpoint rehydrates a single endpoint's persistent DHCP client.
func (p *Plugin) restoreEndpoint(ctx context.Context, networkID, ctrID string, epInfo dTypes.EndpointResource, opts DHCPNetworkOptions) error {
	// If a fresh Join has already registered this endpoint while Restore was
	// running, leave it alone — starting a second udhcpc on the same interface
	// would create duplicate lease traffic.
	p.mu.Lock()
	_, exists := p.persistentDHCP[epInfo.EndpointID]
	p.mu.Unlock()
	if exists {
		return nil
	}

	ctr, err := p.docker.ContainerInspect(ctx, ctrID)
	if err != nil {
		return fmt.Errorf("inspect container: %w", err)
	}
	if ctr.State == nil || !ctr.State.Running || ctr.State.Pid == 0 {
		// Stopped/paused containers don't need a DHCP client; Docker will call
		// CreateEndpoint/Join again if/when they restart.
		return nil
	}

	nsPath := fmt.Sprintf("/proc/%v/ns/net", ctr.State.Pid)
	nsHandle, err := netns.GetFromPath(nsPath)
	if err != nil {
		return fmt.Errorf("open netns: %w", err)
	}
	netHandle, err := netlink.NewHandleAt(nsHandle)
	if err != nil {
		nsHandle.Close()
		return fmt.Errorf("open netlink in netns: %w", err)
	}

	cleanup := func() {
		netHandle.Delete()
		nsHandle.Close()
	}

	ctrLink, ifIndex, err := p.findEndpointLink(netHandle, epInfo, opts)
	if err != nil {
		cleanup()
		return fmt.Errorf("find endpoint interface: %w", err)
	}

	v4Addrs, err := netHandle.AddrList(ctrLink, netlinkFamilyV4)
	if err != nil {
		cleanup()
		return fmt.Errorf("list v4 addrs: %w", err)
	}
	var lastV4 *netlink.Addr
	for i := range v4Addrs {
		// Pick the first global-scope address; skip link-local.
		if v4Addrs[i].IP.IsGlobalUnicast() {
			lastV4 = &v4Addrs[i]
			break
		}
	}
	if lastV4 == nil {
		cleanup()
		return fmt.Errorf("no IPv4 address found on interface")
	}

	var lastV6 *netlink.Addr
	if opts.IPv6 {
		v6Addrs, err := netHandle.AddrList(ctrLink, netlinkFamilyV6)
		if err != nil {
			cleanup()
			return fmt.Errorf("list v6 addrs: %w", err)
		}
		for i := range v6Addrs {
			if v6Addrs[i].IP.IsGlobalUnicast() {
				lastV6 = &v6Addrs[i]
				break
			}
		}
	}

	hostname := ""
	if ctr.Config != nil {
		hostname = ctr.Config.Hostname
	}

	m := newDHCPManager(p.docker, JoinRequest{
		NetworkID:  networkID,
		EndpointID: epInfo.EndpointID,
		// SandboxKey is not used downstream for restore — udhcpc references the netns via nsPath.
	}, opts)
	m.LastIP = lastV4
	m.LastIPv6 = lastV6
	m.IfIndex = ifIndex
	m.nsPath = nsPath
	m.hostname = hostname
	m.nsHandle = nsHandle
	m.netHandle = netHandle
	m.ctrLink = ctrLink

	if err := m.RestoreClient(ctx); err != nil {
		// RestoreClient closes nsHandle/netHandle on its own error path
		return fmt.Errorf("start clients: %w", err)
	}

	p.mu.Lock()
	p.persistentDHCP[epInfo.EndpointID] = m
	p.mu.Unlock()

	log.WithFields(log.Fields{
		"network":  networkID[:12],
		"endpoint": epInfo.EndpointID[:12],
		"ip":       lastV4.String(),
	}).Info("Restore: re-attached persistent DHCP client")
	return nil
}

// findEndpointLink locates the container-side interface inside the given netns.
// In bridge mode it uses the host-side veth pair name to find the peer index.
// In macvlan/ipvlan mode it matches by the endpoint's MAC address.
func (p *Plugin) findEndpointLink(netHandle *netlink.Handle, epInfo dTypes.EndpointResource, opts DHCPNetworkOptions) (netlink.Link, int, error) {
	if opts.NetMode() == NetworkModeBridge {
		hostName, _ := vethPairNames(epInfo.EndpointID)
		hostLink, err := netlink.LinkByName(hostName)
		if err != nil {
			return nil, 0, fmt.Errorf("find host veth %q: %w", hostName, err)
		}
		hostVeth, ok := hostLink.(*netlink.Veth)
		if !ok {
			return nil, 0, util.ErrNotVEth
		}
		peerIdx, err := netlink.VethPeerIndex(hostVeth)
		if err != nil {
			return nil, 0, fmt.Errorf("get veth peer index: %w", err)
		}
		link, err := netHandle.LinkByIndex(peerIdx)
		if err != nil {
			return nil, 0, fmt.Errorf("find peer link by index %d: %w", peerIdx, err)
		}
		return link, peerIdx, nil
	}

	// macvlan/ipvlan: match by MAC. EndpointResource.MacAddress is the address
	// libnetwork assigned during CreateEndpoint, which we wrote on the interface.
	wantMAC := epInfo.MacAddress
	if wantMAC == "" {
		return nil, 0, fmt.Errorf("endpoint has no MAC address recorded")
	}
	links, err := netHandle.LinkList()
	if err != nil {
		return nil, 0, fmt.Errorf("list links in netns: %w", err)
	}
	for _, l := range links {
		if l.Attrs().HardwareAddr.String() == wantMAC {
			return l, l.Attrs().Index, nil
		}
	}
	return nil, 0, fmt.Errorf("no interface with MAC %s", wantMAC)
}

// netlinkFamilyV4/V6 are the address family constants used by netlink.AddrList.
// Defined here as constants so we don't need to pull in golang.org/x/sys/unix
// just for the two values.
const (
	netlinkFamilyV4 = 2  // unix.AF_INET
	netlinkFamilyV6 = 10 // unix.AF_INET6
)

// Listen starts the plugin server
func (p *Plugin) Listen(bindSock string) error {
	l, err := net.Listen("unix", bindSock)
	if err != nil {
		return err
	}

	return p.server.Serve(l)
}

// Close stops the plugin server
func (p *Plugin) Close() error {
	if err := p.docker.Close(); err != nil {
		return fmt.Errorf("failed to close docker client: %w", err)
	}

	if err := p.server.Close(); err != nil {
		return fmt.Errorf("failed to close http server: %w", err)
	}

	return nil
}
