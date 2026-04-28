package plugin

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"

	docker "github.com/moby/moby/client"
	"github.com/gorilla/handlers"
	"github.com/mitchellh/mapstructure"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/fernandodelucca/docker-network-dhcp/pkg/util"
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

// Match both the current name (docker-network-dhcp) and the historical
// short form (docker-net-dhcp) so existing networks created before the
// rename are still recognised by Restore and the bridge-conflict check.
var driverRegexp = regexp.MustCompile(`(^|/)docker-net(work)?-dhcp:.+$`)

// IsDHCPPlugin checks if a Docker network driver is an instance of this plugin
func IsDHCPPlugin(driver string) bool {
	return driverRegexp.MatchString(driver)
}

// shortID returns the first 12 characters of an ID string, safe for IDs shorter than 12 chars
func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
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
	// state mirrors what's been persisted to disk under stateDir/stateFile.
	// Keyed by EndpointID. Rewritten atomically whenever Join/Leave runs so
	// that a restart can rebuild persistentDHCP without depending on the
	// Docker API being responsive (which can deadlock during plugin loading).
	state         map[string]endpointState
	cancelJoin    map[string]context.CancelFunc
	leftEndpoints map[string]struct{}
	// networks caches per-network driver options (Bridge/Mode/IPv6) keyed by
	// NetworkID. Populated on CreateNetwork and persisted to networks.json so
	// that netOptions() can answer Join calls during dockerd boot — before the
	// Docker API is responsive — without blocking or deadlocking.
	networks map[string]networkState

	// saveMu serializes disk writes so concurrent addStateEntry/removeStateEntry
	// calls cannot race on the state file (both take snapshots under mu, but
	// without saveMu the slower write could overwrite the more recent snapshot).
	saveMu sync.Mutex

	restoreCancel   context.CancelFunc
	restoreWg       sync.WaitGroup
	restoreComplete bool
}

// NewPlugin creates a new Plugin. CRITICAL: this function MUST NOT block waiting
// for the Docker daemon to be ready. dockerd starts plugins early in its boot
// sequence and watches for the plugin's unix socket to appear. If we block here,
// dockerd times out waiting for the socket and marks the plugin as failed —
// after a reboot the plugin will come back DISABLED, breaking every container
// that depends on this network.
//
// The Docker client object created here is lazy: it only connects on first API
// call. Background readiness monitoring and operation-level retries handle slow
// dockerd boot transparently.
func NewPlugin(awaitTimeout time.Duration) (*Plugin, error) {
	// 60s is generous on purpose: during a `dockerd` restart or under post-boot
	// IO/CPU pressure, the API socket can be slow for tens of seconds.
	client, err := docker.NewClientWithOpts(
		docker.WithAPIVersionNegotiation(),
		docker.WithTimeout(60*time.Second),
		docker.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	persisted, err := loadState()
	if err != nil {
		// Don't fail startup over a corrupt state file: log and continue with
		// an empty table. The next Join/Leave will rewrite the file from
		// in-memory state, which is the source of truth at runtime.
		log.WithError(err).Warn("Failed to load persisted state — starting empty")
		persisted = map[string]endpointState{}
	} else {
		log.WithField("persisted_endpoints", len(persisted)).Debug("Loaded persisted endpoint state from disk")
	}

	nets, err := loadNetworks()
	if err != nil {
		log.WithError(err).Warn("Failed to load persisted network state — starting empty")
		nets = map[string]networkState{}
	} else {
		log.WithField("persisted_networks", len(nets)).Debug("Loaded persisted network state from disk")
	}

	p := Plugin{
		awaitTimeout: awaitTimeout,

		docker: client,

		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
		state:          persisted,
		cancelJoin:     make(map[string]context.CancelFunc),
		leftEndpoints:  make(map[string]struct{}),
		networks:       nets,
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

	mux.HandleFunc("/healthz", p.apiHealthz)

	// Optional libnetwork driver methods we don't implement. Returning 200
	// with {} (instead of letting ServeMux 404 the path) keeps dockerd from
	// logging trace-level "method not supported" lines for every endpoint.
	//   - {Program,Revoke}ExternalConnectivity: NAT / port-map setup. Not
	//     applicable here — DHCP'd containers sit directly on the physical
	//     LAN, no NAT or port mapping involved.
	//   - {Allocate,Free}Network: only used by drivers with cluster scope.
	//   - Discover{New,Delete}: peer/service discovery for overlay drivers.
	noop := func(w http.ResponseWriter, r *http.Request) {
		util.JSONResponse(w, struct{}{}, http.StatusOK)
	}
	mux.HandleFunc("/NetworkDriver.ProgramExternalConnectivity", noop)
	mux.HandleFunc("/NetworkDriver.RevokeExternalConnectivity", noop)
	mux.HandleFunc("/NetworkDriver.AllocateNetwork", noop)
	mux.HandleFunc("/NetworkDriver.FreeNetwork", noop)
	mux.HandleFunc("/NetworkDriver.DiscoverNew", noop)
	mux.HandleFunc("/NetworkDriver.DiscoverDelete", noop)

	p.server = http.Server{
		Handler: handlers.CustomLoggingHandler(nil, mux, util.WriteAccessLog),
	}

	return &p, nil
}

// addStateEntry persists a new endpoint record to disk. Logs but does not
// return errors: failing to persist is bad, but it must not break the live
// Join — connectivity right now matters more than a clean restart later.
func (p *Plugin) addStateEntry(e endpointState) {
	p.mu.Lock()
	p.state[e.EndpointID] = e
	snapshot := p.copyState()
	p.mu.Unlock()

	p.saveMu.Lock()
	err := saveState(snapshot)
	p.saveMu.Unlock()

	if err != nil {
		log.WithError(err).WithField("endpoint", shortID(e.EndpointID)).
			Warn("Failed to persist endpoint state")
	}
}

// updateStateEntry applies an in-place update to an existing entry, then
// persists. Used by the Join goroutine to add hostname after async resolution.
func (p *Plugin) updateStateEntry(endpointID string, update func(*endpointState)) {
	p.mu.Lock()
	if e, ok := p.state[endpointID]; ok {
		update(&e)
		p.state[endpointID] = e
	}
	snapshot := p.copyState()
	p.mu.Unlock()

	p.saveMu.Lock()
	err := saveState(snapshot)
	p.saveMu.Unlock()

	if err != nil {
		log.WithError(err).WithField("endpoint", shortID(endpointID)).
			Warn("Failed to update persisted state")
	}
}

// removeStateEntry drops an endpoint record from disk. Also cleans
// leftEndpoints to prevent unbounded map growth. Same logging-only policy
// as addStateEntry.
func (p *Plugin) removeStateEntry(endpointID string) {
	p.mu.Lock()
	_, exists := p.state[endpointID]
	if exists {
		delete(p.state, endpointID)
	}
	// Always clean leftEndpoints to prevent map from growing indefinitely for
	// endpoints that had Leave called without an active manager.
	delete(p.leftEndpoints, endpointID)
	snapshot := p.copyState()
	p.mu.Unlock()

	if !exists {
		return
	}

	p.saveMu.Lock()
	err := saveState(snapshot)
	p.saveMu.Unlock()

	if err != nil {
		log.WithError(err).WithField("endpoint", shortID(endpointID)).
			Warn("Failed to update persisted state on remove")
	}
}

// copyState returns a shallow copy of p.state. Must be called with p.mu held.
func (p *Plugin) copyState() map[string]endpointState {
	snapshot := make(map[string]endpointState, len(p.state))
	for k, v := range p.state {
		snapshot[k] = v
	}
	return snapshot
}

// addNetwork persists network driver options so netOptions() can answer Join
// requests without the Docker API during dockerd boot.
func (p *Plugin) addNetwork(ns networkState) {
	p.mu.Lock()
	p.networks[ns.NetworkID] = ns
	snapshot := p.copyNetworks()
	p.mu.Unlock()

	p.saveMu.Lock()
	err := saveNetworks(snapshot)
	p.saveMu.Unlock()

	if err != nil {
		log.WithError(err).WithField("network", shortID(ns.NetworkID)).
			Warn("Failed to persist network state")
	}
}

// removeNetwork drops a network record from the networks cache.
func (p *Plugin) removeNetwork(networkID string) {
	p.mu.Lock()
	_, exists := p.networks[networkID]
	if exists {
		delete(p.networks, networkID)
	}
	snapshot := p.copyNetworks()
	p.mu.Unlock()

	if !exists {
		return
	}

	p.saveMu.Lock()
	err := saveNetworks(snapshot)
	p.saveMu.Unlock()

	if err != nil {
		log.WithError(err).WithField("network", shortID(networkID)).
			Warn("Failed to update persisted network state on remove")
	}
}

// copyNetworks returns a shallow copy of p.networks. Must be called with p.mu held.
func (p *Plugin) copyNetworks() map[string]networkState {
	snapshot := make(map[string]networkState, len(p.networks))
	for k, v := range p.networks {
		snapshot[k] = v
	}
	return snapshot
}

// Restore re-attaches persistent DHCP clients to endpoints that survived a
// plugin restart. It is driven entirely by the persisted state file — no calls
// to the Docker API — so it cannot deadlock against dockerd while dockerd is
// holding its plugin-loading lock. Endpoints whose sandbox netns is gone (e.g.
// after a host reboot) are silently dropped from the state file.
func (p *Plugin) Restore(ctx context.Context) error {
	p.mu.Lock()
	snapshot := make([]endpointState, 0, len(p.state))
	for _, e := range p.state {
		snapshot = append(snapshot, e)
	}
	p.mu.Unlock()

	if len(snapshot) == 0 {
		log.Info("Restore complete (no persisted state)")
		return nil
	}

	log.WithField("total_endpoints", len(snapshot)).Info("Restore: starting recovery of persisted endpoints")

	restoreBackoffs := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

	restored := 0
	dropped := 0
	for _, e := range snapshot {
		if err := ctx.Err(); err != nil {
			log.WithError(err).Info("Restore: cancelled by context")
			return err
		}
		log.WithFields(log.Fields{
			"network":  shortID(e.NetworkID),
			"endpoint": shortID(e.EndpointID),
			"sandbox":  e.SandboxKey,
			"mode":     e.Mode,
			"bridge":   e.Bridge,
		}).Trace("Restore: attempting to recover endpoint")

		var restoreErr error
		for attempt := 0; attempt <= len(restoreBackoffs); attempt++ {
			if attempt > 0 {
				delay := restoreBackoffs[attempt-1]
				log.WithFields(log.Fields{
					"endpoint": shortID(e.EndpointID),
					"attempt":  attempt,
					"delay":    delay,
				}).Debug("Restore: retrying after transient failure")
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					log.WithError(ctx.Err()).Info("Restore: cancelled during retry wait")
					return ctx.Err()
				}
			}
			restoreErr = p.restoreFromState(ctx, e)
			if restoreErr == nil {
				break
			}
		}

		if restoreErr != nil {
			log.WithError(restoreErr).WithFields(log.Fields{
				"network":  shortID(e.NetworkID),
				"endpoint": shortID(e.EndpointID),
				"sandbox":  e.SandboxKey,
			}).Warn("Restore: dropping endpoint after retries exhausted")
			p.removeStateEntry(e.EndpointID)
			dropped++
			continue
		}
		restored++
	}
	log.WithFields(log.Fields{
		"restored": restored,
		"dropped":  dropped,
		"total":    len(snapshot),
	}).Info("Restore complete")
	return nil
}

// restoreFromState rebuilds a single dhcpManager from a persisted record.
func (p *Plugin) restoreFromState(ctx context.Context, e endpointState) error {
	logFields := log.Fields{
		"network":  shortID(e.NetworkID),
		"endpoint": shortID(e.EndpointID),
		"sandbox":  e.SandboxKey,
	}

	// If a fresh Join already registered this endpoint, don't double-attach.
	p.mu.Lock()
	_, exists := p.persistentDHCP[e.EndpointID]
	p.mu.Unlock()
	if exists {
		log.WithFields(logFields).Trace("Restore: endpoint already restored by concurrent Join")
		return nil
	}

	// SandboxKey points at the container's netns. After a reboot the tmpfs
	// entry is gone; that's how we detect "container is gone" without needing
	// the Docker API.
	log.WithFields(logFields).Trace("Restore: opening network namespace")
	nsHandle, err := netns.GetFromPath(e.SandboxKey)
	if err != nil {
		return fmt.Errorf("open netns %s: %w", e.SandboxKey, err)
	}

	log.WithFields(logFields).Trace("Restore: opening netlink handle in namespace")
	netHandle, err := netlink.NewHandleAt(nsHandle)
	if err != nil {
		nsHandle.Close()
		return fmt.Errorf("open netlink in netns: %w", err)
	}
	cleanup := func() {
		netHandle.Delete()
		nsHandle.Close()
	}

	log.WithFields(logFields).Trace("Restore: locating endpoint interface")
	ctrLink, ifIndex, err := findEndpointLinkFromState(netHandle, e)
	if err != nil {
		cleanup()
		return fmt.Errorf("find endpoint interface: %w", err)
	}

	log.WithFields(logFields).Trace("Restore: reading current IPv4 address")
	v4Addrs, err := netHandle.AddrList(ctrLink, netlinkFamilyV4)
	if err != nil {
		cleanup()
		return fmt.Errorf("list v4 addrs: %w", err)
	}
	var lastV4 *netlink.Addr
	for i := range v4Addrs {
		if v4Addrs[i].IP.IsGlobalUnicast() {
			lastV4 = &v4Addrs[i]
			break
		}
	}
	if lastV4 == nil {
		cleanup()
		return fmt.Errorf("no IPv4 address on interface")
	}
	log.WithFields(logFields).WithField("ipv4", lastV4.String()).Trace("Restore: IPv4 address found")

	var lastV6 *netlink.Addr
	if e.IPv6 {
		log.WithFields(logFields).Trace("Restore: reading current IPv6 address")
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
		if lastV6 != nil {
			log.WithFields(logFields).WithField("ipv6", lastV6.String()).Trace("Restore: IPv6 address found")
		}
	}

	opts := DHCPNetworkOptions{
		Bridge: e.Bridge,
		Mode:   e.Mode,
		IPv6:   e.IPv6,
	}

	log.WithFields(logFields).WithField("mode", e.Mode).Trace("Restore: creating DHCP manager")
	m := newDHCPManager(p.docker, JoinRequest{
		NetworkID:  e.NetworkID,
		EndpointID: e.EndpointID,
		SandboxKey: e.SandboxKey,
	}, opts)
	m.LastIP = lastV4
	m.LastIPv6 = lastV6
	m.IfIndex = ifIndex
	m.nsPath = e.SandboxKey
	m.hostname = e.Hostname
	m.nsHandle = nsHandle
	m.netHandle = netHandle
	m.ctrLink = ctrLink

	log.WithFields(logFields).Trace("Restore: starting DHCP clients")
	if err := m.RestoreClient(ctx); err != nil {
		// RestoreClient closes nsHandle/netHandle on its own error path
		return fmt.Errorf("start clients: %w", err)
	}

	// Re-check under lock to detect concurrent Join or Leave. If Join won the race,
	// we stop our restored manager and return nil (Join's manager is already active).
	// If Leave marked the endpoint during restore, we stop the manager and return error.
	p.mu.Lock()
	if _, exists := p.persistentDHCP[e.EndpointID]; exists {
		p.mu.Unlock()
		log.WithFields(logFields).Debug("Restore: concurrent Join arrived, discarding our restored manager")
		_ = m.Stop() // Join concurrent arrived first; discard our restored manager
		return nil
	}
	if _, left := p.leftEndpoints[e.EndpointID]; left {
		delete(p.leftEndpoints, e.EndpointID)
		p.mu.Unlock()
		log.WithFields(logFields).Debug("Restore: endpoint was left during restore, stopping manager")
		_ = m.Stop()
		return fmt.Errorf("endpoint was left during restore")
	}
	p.persistentDHCP[e.EndpointID] = m
	p.mu.Unlock()

	log.WithFields(logFields).WithFields(log.Fields{
		"ipv4": lastV4.String(),
		"ipv6": func() string {
			if lastV6 != nil {
				return lastV6.String()
			}
			return "none"
		}(),
	}).Info("Restore: re-attached persistent DHCP client successfully")
	return nil
}

// findEndpointLinkFromState locates the container-side interface inside the
// given netns using only data from the persisted state record.
//   - bridge: derive the host-side veth name and follow its peer index.
//   - macvlan/ipvlan: use the recorded IfIndex (preserved across netns moves).
func findEndpointLinkFromState(netHandle *netlink.Handle, e endpointState) (netlink.Link, int, error) {
	mode := e.Mode
	if mode == "" {
		mode = NetworkModeBridge
	}
	if mode == NetworkModeBridge {
		hostName, _ := vethPairNames(e.EndpointID)
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
		if peerIdx == 0 {
			return nil, 0, fmt.Errorf("veth peer index is 0 — peer may be destroyed")
		}
		link, err := netHandle.LinkByIndex(peerIdx)
		if err != nil {
			return nil, 0, fmt.Errorf("find peer link by index %d in netns: %w", peerIdx, err)
		}
		return link, peerIdx, nil
	}

	// macvlan/ipvlan
	if e.IfIndex == 0 {
		return nil, 0, fmt.Errorf("no interface index recorded for %s endpoint", mode)
	}
	link, err := netHandle.LinkByIndex(e.IfIndex)
	if err != nil {
		return nil, 0, fmt.Errorf("find link by index %d in netns: %w", e.IfIndex, err)
	}
	return link, e.IfIndex, nil
}

// netlinkFamilyV4/V6 are the address family constants used by netlink.AddrList.
// netlinkRTN_UNICAST and netlinkRTPROT_KERNEL are route type/protocol constants.
// Defined here as constants so we don't need to pull in golang.org/x/sys/unix.
const (
	netlinkFamilyV4      = 2  // unix.AF_INET
	netlinkFamilyV6      = 10 // unix.AF_INET6
	netlinkRTN_UNICAST   = 1  // unix.RTN_UNICAST
	netlinkRTPROT_KERNEL = 2  // unix.RTPROT_KERNEL
)


// Client returns the Docker client for readiness probing
func (p *Plugin) Client() *docker.Client {
	return p.docker
}

// Listen starts the plugin server
func (p *Plugin) Listen(bindSock string) error {
	l, err := net.Listen("unix", bindSock)
	if err != nil {
		return err
	}

	return p.server.Serve(l)
}

// StartRestore launches the Restore goroutine with a 5-minute deadline and 10-second boot delay.
// It holds the cancel func internally so Close() can cancel in-flight restore operations.
func (p *Plugin) StartRestore() {
	p.restoreWg.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	p.mu.Lock()
	p.restoreCancel = cancel
	p.mu.Unlock()
	go func() {
		defer p.restoreWg.Done()
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				log.WithField("panic", fmt.Sprintf("%v", r)).Error("CRITICAL: Restore goroutine panicked — recovered from panic")
			}
			p.mu.Lock()
			p.restoreCancel = nil
			p.mu.Unlock()
		}()
		log.Info("Restore phase: waiting 10s for dockerd to load containers...")
		time.Sleep(10 * time.Second)
		log.Info("Restore phase: starting recovery of persisted endpoints")
		if err := p.Restore(ctx); err != nil {
			log.WithError(err).Error("Restore phase reported errors")
		}
		p.mu.Lock()
		p.restoreComplete = true
		p.mu.Unlock()
	}()
}

// Close stops the plugin server, cancels in-flight restore, and stops all DHCP managers
func (p *Plugin) Close() error {
	// 1. Cancel restore in-flight and wait for it to finish
	p.mu.Lock()
	if p.restoreCancel != nil {
		p.restoreCancel()
	}
	p.mu.Unlock()
	p.restoreWg.Wait()

	// 2. Stop all active DHCP managers
	p.mu.Lock()
	managers := make([]*dhcpManager, 0, len(p.persistentDHCP))
	for _, m := range p.persistentDHCP {
		managers = append(managers, m)
	}
	p.mu.Unlock()
	for _, m := range managers {
		if err := m.Stop(); err != nil {
			log.WithError(err).Warn("Error stopping DHCP manager during shutdown")
		}
	}

	// 3. Close HTTP server first, then Docker client
	if err := p.server.Close(); err != nil {
		return fmt.Errorf("failed to close http server: %w", err)
	}
	if err := p.docker.Close(); err != nil {
		return fmt.Errorf("failed to close docker client: %w", err)
	}

	return nil
}

// healthzResponse is the JSON body returned by /healthz.
type healthzResponse struct {
	Status          string `json:"status"`
	DockerConnected bool   `json:"docker_connected"`
	Endpoints       int    `json:"endpoints"`
	RestoreComplete bool   `json:"restore_complete"`
}

// apiHealthz handles GET /healthz. Accessible via the plugin unix socket:
//
//	curl --unix-socket /run/docker/plugins/<ID>/net-dhcp.sock http://localhost/healthz
func (p *Plugin) apiHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
	defer cancel()

	_, pingErr := p.docker.Ping(ctx, docker.PingOptions{})
	dockerOK := pingErr == nil

	p.mu.Lock()
	endpoints := len(p.persistentDHCP)
	restoreComplete := p.restoreComplete
	p.mu.Unlock()

	status := "ok"
	if !dockerOK {
		status = "degraded"
	}

	code := http.StatusOK
	if !dockerOK {
		code = http.StatusServiceUnavailable
	}

	util.JSONResponse(w, healthzResponse{
		Status:          status,
		DockerConnected: dockerOK,
		Endpoints:       endpoints,
		RestoreComplete: restoreComplete,
	}, code)
}
