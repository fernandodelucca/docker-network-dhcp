package plugin

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"time"

	docker "github.com/moby/moby/client"
	"github.com/mitchellh/mapstructure"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/fernandodelucca/docker-network-dhcp/pkg/udhcpc"
	"github.com/fernandodelucca/docker-network-dhcp/pkg/util"
)

// CLIOptionsKey is the key used in create network options by the CLI for custom options
const CLIOptionsKey string = "com.docker.network.generic"

// Implementations of the endpoints described in
// https://github.com/moby/libnetwork/blob/master/docs/remote.md

// CreateNetwork "creates" a new DHCP network (validates the interface and IPAM settings)
func (p *Plugin) CreateNetwork(r CreateNetworkRequest) error {
	log.WithField("network_id", shortID(r.NetworkID)).Debug("CreateNetwork: decoding options")

	opts, err := decodeOpts(r.Options[util.OptionsKeyGeneric])
	if err != nil {
		return fmt.Errorf("failed to decode network options: %w", err)
	}

	log.WithFields(log.Fields{
		"network_id":      shortID(r.NetworkID),
		"bridge":          opts.Bridge,
		"mode":            opts.NetMode(),
		"ipv6":            opts.IPv6,
		"lease_timeout":   opts.LeaseTimeout,
		"skip_routes":     opts.SkipRoutes,
		"ignore_conflicts": opts.IgnoreConflicts,
	}).Debug("CreateNetwork: decoded options")

	switch opts.NetMode() {
	case NetworkModeBridge, NetworkModeMacvlan, NetworkModeIPvlan:
		// valid
	default:
		return util.ErrInvalidMode
	}

	if opts.Bridge == "" {
		return util.ErrBridgeRequired
	}

	for _, d := range r.IPv4Data {
		if d.AddressSpace != "null" || d.Pool != "0.0.0.0/0" {
			return util.ErrIPAM
		}
	}

	link, err := netlink.LinkByName(opts.Bridge)
	if err != nil {
		return fmt.Errorf("failed to lookup interface %v: %w", opts.Bridge, err)
	}

	if opts.NetMode() == NetworkModeBridge {
		if link.Type() != "bridge" {
			return util.ErrNotBridge
		}

		if !opts.IgnoreConflicts {
			v4Addrs, err := netlink.AddrList(link, netlinkFamilyV4)
			if err != nil {
				return fmt.Errorf("failed to retrieve IPv4 addresses for %v: %w", opts.Bridge, err)
			}
			v6Addrs, err := netlink.AddrList(link, netlinkFamilyV6)
			if err != nil {
				return fmt.Errorf("failed to retrieve IPv6 addresses for %v: %w", opts.Bridge, err)
			}
			bridgeAddrs := append(v4Addrs, v6Addrs...)

			netsResult, err := p.docker.NetworkList(context.Background(), docker.NetworkListOptions{})
			if err != nil {
				return fmt.Errorf("failed to retrieve list of networks from Docker: %w", err)
			}

			// Make sure the addresses on this bridge aren't used by another network
			for _, n := range netsResult.Items {
				if IsDHCPPlugin(n.Driver) {
					otherOpts, err := decodeOpts(n.Options)
					if err != nil {
						log.
							WithField("network", n.Name).
							WithError(err).
							Warn("Failed to parse other DHCP network's options")
					} else if otherOpts.Bridge == opts.Bridge {
						return util.ErrBridgeUsed
					}
				}
				if n.IPAM.Driver == "null" {
					// Null driver networks will have 0.0.0.0/0 which covers any address range!
					continue
				}

				for _, c := range n.IPAM.Config {
					_, dockerCIDR, err := net.ParseCIDR(c.Subnet.String())
					if err != nil {
						return fmt.Errorf("failed to parse subnet %v on Docker network %v: %w", c.Subnet, n.ID, err)
					}
					if bytes.Equal(dockerCIDR.Mask, net.CIDRMask(0, 32)) || bytes.Equal(dockerCIDR.Mask, net.CIDRMask(0, 128)) {
						// Last check to make sure the network isn't 0.0.0.0/0 or ::/0 (which would always pass the check below)
						continue
					}

					for _, bridgeAddr := range bridgeAddrs {
						if bridgeAddr.IPNet.Contains(dockerCIDR.IP) || dockerCIDR.Contains(bridgeAddr.IP) {
							return util.ErrBridgeUsed
						}
					}
				}
			}
		}
	}

	log.WithFields(log.Fields{
		"network": r.NetworkID,
		"bridge":  opts.Bridge,
		"mode":    opts.NetMode(),
		"ipv6":    opts.IPv6,
	}).Info("Network created")

	return nil
}

// DeleteNetwork "deletes" a DHCP network (does nothing, the bridge is managed by the user)
func (p *Plugin) DeleteNetwork(r DeleteNetworkRequest) error {
	log.WithField("network", r.NetworkID).Info("Network deleted")
	return nil
}

func vethPairNames(id string) (string, string) {
	return "dh-" + id[:12], id[:12] + "-dh"
}

// netOptions resolves the DHCP network options for a given network. It tries
// the Docker API first (authoritative) and falls back to persisted endpoint
// state if the API is unavailable.
//
// The fallback is critical during dockerd boot: dockerd may invoke our Join
// handler before its own API is fully responsive, but it does have a stable
// libnetwork state for any pre-existing endpoint. By caching the per-network
// options through the persisted endpoint records (Mode/Bridge/IPv6), we can
// answer Join even when Docker API is briefly unreachable, allowing containers
// with restart=always to come back up after a server reboot without manual
// intervention.
func (p *Plugin) netOptions(ctx context.Context, id string) (DHCPNetworkOptions, error) {
	dummy := DHCPNetworkOptions{}

	nResult, err := p.docker.NetworkInspect(ctx, id, docker.NetworkInspectOptions{})
	if err == nil {
		opts, decodeErr := decodeOpts(nResult.Network.Options)
		if decodeErr != nil {
			return dummy, fmt.Errorf("failed to parse options: %w", decodeErr)
		}
		return opts, nil
	}

	// Docker API failed. Try to reconstruct from any persisted endpoint that
	// belongs to this network. Bridge/Mode/IPv6 are persisted per-endpoint and
	// they are identical for every endpoint in the same network.
	p.mu.Lock()
	var cached *endpointState
	for _, e := range p.state {
		if e.NetworkID == id {
			eCopy := e
			cached = &eCopy
			break
		}
	}
	p.mu.Unlock()

	if cached != nil {
		log.WithError(err).WithFields(log.Fields{
			"network": shortID(id),
			"bridge":  cached.Bridge,
			"mode":    cached.Mode,
		}).Warn("Docker API unavailable for netOptions — using cached state from persisted endpoint")
		return DHCPNetworkOptions{
			Bridge: cached.Bridge,
			Mode:   cached.Mode,
			IPv6:   cached.IPv6,
		}, nil
	}

	return dummy, fmt.Errorf("failed to get info from Docker: %w", err)
}

// CreateEndpoint creates the container-side network interface and uses udhcpc to acquire an initial IP address.
// In bridge mode a veth pair is used; in macvlan/ipvlan mode a directly-attached sub-interface is created on the
// parent NIC. Docker will move the interface into the container's namespace and apply the address.
func (p *Plugin) CreateEndpoint(ctx context.Context, r CreateEndpointRequest) (CreateEndpointResponse, error) {
	startTime := time.Now()
	logFields := log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
	}
	log.WithFields(logFields).Debug("CreateEndpoint: starting")

	res := CreateEndpointResponse{
		Interface: &EndpointInterface{},
	}

	if r.Interface != nil && (r.Interface.Address != "" || r.Interface.AddressIPv6 != "") {
		// TODO: Should we allow static IP's somehow?
		return res, util.ErrIPAM
	}

	opts, err := p.netOptions(ctx, r.NetworkID)
	if err != nil {
		return res, fmt.Errorf("failed to get network options: %w", err)
	}
	log.WithFields(logFields).WithField("mode", opts.NetMode()).Debug("CreateEndpoint: using network mode")

	timeout := defaultLeaseTimeout
	if opts.LeaseTimeout != 0 {
		timeout = opts.LeaseTimeout
	}
	log.WithFields(logFields).WithField("lease_timeout", timeout).Debug("CreateEndpoint: lease timeout configured")

	// initialIP runs udhcpc on ifName to obtain an IP and stores it in joinHints.
	initialIP := func(ifName string, v6 bool) error {
		v6str := ""
		if v6 {
			v6str = "v6"
		}

		timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		info, err := udhcpc.GetIP(timeoutCtx, ifName, &udhcpc.DHCPClientOptions{V6: v6})
		if err != nil {
			return fmt.Errorf("failed to get initial IP%v address via DHCP%v: %w", v6str, v6str, err)
		}
		ip, err := netlink.ParseAddr(info.IP)
		if err != nil {
			return fmt.Errorf("failed to parse initial IP%v address: %w", v6str, err)
		}

		p.mu.Lock()
		hint := p.joinHints[r.EndpointID]
		if v6 {
			res.Interface.AddressIPv6 = info.IP
			hint.IPv6 = ip
			// No gateways in DHCPv6!
		} else {
			res.Interface.Address = info.IP
			hint.IPv4 = ip
			hint.Gateway = info.Gateway
		}
		p.joinHints[r.EndpointID] = hint
		p.mu.Unlock()

		return nil
	}

	if opts.NetMode() == NetworkModeBridge {
		bridge, err := netlink.LinkByName(opts.Bridge)
		if err != nil {
			return res, fmt.Errorf("failed to get bridge interface: %w", err)
		}

		hostName, ctrName := vethPairNames(r.EndpointID)
		la := netlink.NewLinkAttrs()
		la.Name = hostName
		hostLink := &netlink.Veth{
			LinkAttrs: la,
			PeerName:  ctrName,
		}
		if r.Interface.MacAddress != "" {
			addr, err := net.ParseMAC(r.Interface.MacAddress)
			if err != nil {
				return res, util.ErrMACAddress
			}

			hostLink.PeerHardwareAddr = addr
		}

		if err := netlink.LinkAdd(hostLink); err != nil {
			return res, fmt.Errorf("failed to create veth pair: %w", err)
		}
		if err := func() error {
			if err := netlink.LinkSetUp(hostLink); err != nil {
				return fmt.Errorf("failed to set host side link of veth pair up: %w", err)
			}

			ctrLink, err := netlink.LinkByName(ctrName)
			if err != nil {
				return fmt.Errorf("failed to find container side of veth pair: %w", err)
			}
			if err := netlink.LinkSetUp(ctrLink); err != nil {
				return fmt.Errorf("failed to set container side link of veth pair up: %w", err)
			}

			// Only write back the MAC address if it wasn't provided to us by libnetwork
			if r.Interface.MacAddress == "" {
				// The kernel will often reset a randomly assigned MAC address after actions like LinkSetMaster. We prevent
				// this behaviour by setting it manually to the random value
				if err := netlink.LinkSetHardwareAddr(ctrLink, ctrLink.Attrs().HardwareAddr); err != nil {
					return fmt.Errorf("failed to set container side of veth pair's MAC address: %w", err)
				}

				res.Interface.MacAddress = ctrLink.Attrs().HardwareAddr.String()
			}

			if err := netlink.LinkSetMaster(hostLink, bridge); err != nil {
				return fmt.Errorf("failed to attach host side link of veth peer to bridge: %w", err)
			}

			if err := initialIP(ctrName, false); err != nil {
				return err
			}
			if opts.IPv6 {
				if err := initialIP(ctrName, true); err != nil {
					return err
				}
			}

			return nil
		}(); err != nil {
			// Be sure to clean up the veth pair if any of this fails
			netlink.LinkDel(hostLink)
			p.mu.Lock()
			delete(p.joinHints, r.EndpointID)
			p.mu.Unlock()
			return res, err
		}
	} else {
		// macvlan or ipvlan mode: create a sub-interface directly on the parent NIC.
		// The interface is temporarily created in the host namespace so that udhcpc can
		// obtain an initial lease; Docker will move it into the container namespace via Join.
		if opts.NetMode() == NetworkModeIPvlan && r.Interface.MacAddress != "" {
			// ipvlan interfaces share the parent's MAC; a custom MAC is not supported
			return res, util.ErrMACAddress
		}

		parentLink, err := netlink.LinkByName(opts.Bridge)
		if err != nil {
			return res, fmt.Errorf("failed to get parent interface %v: %w", opts.Bridge, err)
		}

		ifName := "dh-" + r.EndpointID[:12]
		la := netlink.NewLinkAttrs()
		la.Name = ifName
		la.ParentIndex = parentLink.Attrs().Index
		if r.Interface.MacAddress != "" {
			addr, err := net.ParseMAC(r.Interface.MacAddress)
			if err != nil {
				return res, util.ErrMACAddress
			}
			la.HardwareAddr = addr
		}

		var newLink netlink.Link
		if opts.NetMode() == NetworkModeMacvlan {
			newLink = &netlink.Macvlan{LinkAttrs: la, Mode: netlink.MACVLAN_MODE_BRIDGE}
		} else {
			newLink = &netlink.IPVlan{LinkAttrs: la, Mode: netlink.IPVLAN_MODE_L2}
		}

		if err := netlink.LinkAdd(newLink); err != nil {
			return res, fmt.Errorf("failed to create %v interface: %w", opts.NetMode(), err)
		}
		if err := func() error {
			// Re-fetch to get the kernel-assigned index and MAC
			ctrLink, err := netlink.LinkByName(ifName)
			if err != nil {
				return fmt.Errorf("failed to get %v interface after creation: %w", opts.NetMode(), err)
			}
			if err := netlink.LinkSetUp(ctrLink); err != nil {
				return fmt.Errorf("failed to set %v interface up: %w", opts.NetMode(), err)
			}

			if opts.NetMode() == NetworkModeMacvlan && r.Interface.MacAddress == "" {
				res.Interface.MacAddress = ctrLink.Attrs().HardwareAddr.String()
			}

			// Store the interface index so the persistent DHCP manager can locate
			// the interface after Docker moves it into the container namespace.
			p.mu.Lock()
			hint := p.joinHints[r.EndpointID]
			hint.IfIndex = ctrLink.Attrs().Index
			p.joinHints[r.EndpointID] = hint
			p.mu.Unlock()

			if err := initialIP(ifName, false); err != nil {
				return err
			}
			if opts.IPv6 {
				if err := initialIP(ifName, true); err != nil {
					return err
				}
			}

			return nil
		}(); err != nil {
			netlink.LinkDel(newLink)
			p.mu.Lock()
			delete(p.joinHints, r.EndpointID)
			p.mu.Unlock()
			return res, err
		}
	}

	p.mu.Lock()
	gateway := p.joinHints[r.EndpointID].Gateway
	p.mu.Unlock()

	duration := time.Since(startTime)
	log.WithFields(logFields).WithFields(log.Fields{
		"mode":         opts.NetMode(),
		"mac_address":  res.Interface.MacAddress,
		"ip":           res.Interface.Address,
		"ipv6":         res.Interface.AddressIPv6,
		"gateway":      fmt.Sprintf("%#v", gateway),
		"duration_ms":  duration.Milliseconds(),
		"skip_routes":  opts.SkipRoutes,
	}).Info("CreateEndpoint: completed successfully")

	return res, nil
}

type operInfo struct {
	Bridge      string `mapstructure:"bridge"`
	HostVEth    string `mapstructure:"veth_host"`
	HostVEthMAC string `mapstructure:"veth_host_mac"`
}

// EndpointOperInfo retrieves some info about an existing endpoint. Reads
// bridge/mode from the persisted state only (no Docker API calls).
// If state is unavailable, returns empty info. This avoids a deadlock seen
// in the wild: libnetwork holds locks while cleaning stale endpoints, blocking
// NetworkInspect for minutes; if we relied on it, EndpointOperInfo would hang
// the whole cleanup loop.
func (p *Plugin) EndpointOperInfo(ctx context.Context, r InfoRequest) (InfoResponse, error) {
	res := InfoResponse{}

	mode, bridge, err := p.endpointModeAndBridge(ctx, r.NetworkID, r.EndpointID)
	if err != nil {
		return res, fmt.Errorf("failed to determine endpoint mode/bridge: %w", err)
	}

	// If no state for this endpoint, return empty info (state may not be persisted yet)
	if mode == "" {
		log.WithFields(log.Fields{
			"network":  r.NetworkID[:12],
			"endpoint": r.EndpointID[:12],
		}).Debug("EndpointOperInfo: no persisted state, returning empty info")
		return InfoResponse{Value: make(map[string]string)}, nil
	}

	info := operInfo{Bridge: bridge}

	if mode == NetworkModeBridge {
		hostName, _ := vethPairNames(r.EndpointID)
		hostLink, err := netlink.LinkByName(hostName)
		if err != nil {
			return res, fmt.Errorf("failed to find host side of veth pair: %w", err)
		}
		info.HostVEth = hostName
		info.HostVEthMAC = hostLink.Attrs().HardwareAddr.String()
	}

	if err := mapstructure.Decode(info, &res.Value); err != nil {
		return res, fmt.Errorf("failed to encode OperInfo: %w", err)
	}

	return res, nil
}

// endpointModeAndBridge returns the network mode and bridge for an endpoint from
// persisted state. Returns empty strings if not found (caller decides on fallback).
// Avoids Docker API calls which can deadlock during libnetwork cleanup.
func (p *Plugin) endpointModeAndBridge(ctx context.Context, networkID, endpointID string) (string, string, error) {
	p.mu.Lock()
	state, ok := p.state[endpointID]
	p.mu.Unlock()
	if !ok {
		return "", "", nil
	}
	mode := state.Mode
	if mode == "" {
		mode = NetworkModeBridge
	}
	return mode, state.Bridge, nil
}

// DeleteEndpoint cleans up the endpoint interface. Bridge mode: delete the
// host-side veth (cascades to the container side). Macvlan/ipvlan mode: the
// interface lives in the container netns and dies when that's torn down.
//
// Critically, this handler must NOT depend on the Docker API. In the field we
// saw dockerd hold libnetwork locks for minutes while cleaning stale
// endpoints, which made every NetworkInspect call from the plugin block until
// the 60s timeout. That blocked DeleteEndpoint, dockerd retried in a 3-minute
// loop, and the host eventually got watchdog-killed. We instead resolve the
// mode from the persisted state (or fall back to "best effort: try to delete
// the veth, ignore if absent") and always return success — DeleteEndpoint is
// inherently a teardown step, so swallowing errors is safer than blocking.
func (p *Plugin) DeleteEndpoint(ctx context.Context, r DeleteEndpointRequest) error {
	logFields := log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
	}
	log.WithFields(logFields).Debug("DeleteEndpoint: starting")

	defer p.removeStateEntry(r.EndpointID)

	p.mu.Lock()
	state, hasState := p.state[r.EndpointID]
	p.mu.Unlock()

	if hasState && state.Mode != "" && state.Mode != NetworkModeBridge {
		log.WithFields(logFields).WithField("mode", state.Mode).Debug("DeleteEndpoint: skipping veth cleanup (interface lives in container netns)")
		return nil
	}

	// Either bridge mode (with state) or unknown (no state) — try to delete
	// the host-side veth. If it doesn't exist we were probably macvlan/ipvlan
	// or the cleanup already happened; either way, nothing to do.
	hostName, _ := vethPairNames(r.EndpointID)
	link, err := netlink.LinkByName(hostName)
	if err != nil {
		log.WithFields(logFields).WithField("veth", hostName).Debug("DeleteEndpoint: no host veth found")
		return nil
	}

	if err := netlink.LinkDel(link); err != nil {
		// Don't fail — best-effort. dockerd is in a teardown path; refusing
		// here just makes it retry forever.
		log.WithError(err).WithFields(logFields).WithField("veth", hostName).Warn("DeleteEndpoint: failed to delete veth (continuing anyway)")
		return nil
	}

	log.WithFields(logFields).Info("DeleteEndpoint: veth deleted successfully")

	return nil
}

func (p *Plugin) addRoutes(opts *DHCPNetworkOptions, v6 bool, bridge netlink.Link, r JoinRequest, hint joinHint, res *JoinResponse) error {
	family := netlinkFamilyV4
	if v6 {
		family = netlinkFamilyV6
	}

	routes, err := netlink.RouteListFiltered(family, &netlink.Route{
		LinkIndex: bridge.Attrs().Index,
		Type:      netlinkRTN_UNICAST,
	}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TYPE)
	if err != nil {
		return fmt.Errorf("failed to list routes: %w", err)
	}

	logFields := log.Fields{
		"network":  r.NetworkID[:12],
		"endpoint": r.EndpointID[:12],
		"sandbox":  r.SandboxKey,
	}
	for _, route := range routes {
		if route.Dst == nil {
			// Default route
			switch family {
			case netlinkFamilyV4:
				if res.Gateway == "" {
					res.Gateway = route.Gw.String()
					log.
						WithFields(logFields).
						WithField("gateway", res.Gateway).
						Info("[Join] Setting IPv4 gateway retrieved from bridge interface on host routing table")
				}
			case netlinkFamilyV6:
				if res.GatewayIPv6 == "" {
					res.GatewayIPv6 = route.Gw.String()
					log.
						WithFields(logFields).
						WithField("gateway", res.GatewayIPv6).
						Info("[Join] Setting IPv6 gateway retrieved from bridge interface on host routing table")
				}
			}

			continue
		}

		if opts.SkipRoutes {
			// Don't do static routes at all
			continue
		}

		if route.Protocol == netlinkRTPROT_KERNEL ||
			(family == netlinkFamilyV4 && route.Dst.Contains(hint.IPv4.IP)) ||
			(family == netlinkFamilyV6 && route.Dst.Contains(hint.IPv6.IP)) {
			// Make sure to leave out the default on-link route created automatically for the IP(s) acquired by DHCP
			continue
		}

		staticRoute := &StaticRoute{
			Destination: route.Dst.String(),
			// Default to an on-link route
			RouteType: 1,
		}
		res.StaticRoutes = append(res.StaticRoutes, staticRoute)

		if route.Gw != nil {
			staticRoute.RouteType = 0
			staticRoute.NextHop = route.Gw.String()

			log.
				WithFields(logFields).
				WithField("route", staticRoute.Destination).
				WithField("gateway", staticRoute.NextHop).
				Info("[Join] Adding route (via gateway) retrieved from bridge interface on host routing table")
		} else {
			log.
				WithFields(logFields).
				WithField("route", staticRoute.Destination).
				Info("[Join] Adding on-link route retrieved from bridge interface on host routing table")
		}
	}

	return nil
}

// Join passes the veth name and route information (gateway from DHCP and existing routes on the host bridge) to Docker
// and starts a persistent DHCP client to maintain the lease on the acquired IP
func (p *Plugin) Join(ctx context.Context, r JoinRequest) (JoinResponse, error) {
	startTime := time.Now()
	logFields := log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
		"sandbox":  r.SandboxKey,
	}
	log.WithFields(logFields).Debug("Join: starting")
	res := JoinResponse{}

	opts, err := p.netOptions(ctx, r.NetworkID)
	if err != nil {
		return res, fmt.Errorf("failed to get network options: %w", err)
	}
	log.WithFields(logFields).WithFields(log.Fields{
		"mode":       opts.NetMode(),
		"skip_routes": opts.SkipRoutes,
	}).Debug("Join: network options loaded")

	var srcName, dstPrefix string
	if opts.NetMode() == NetworkModeBridge {
		_, srcName = vethPairNames(r.EndpointID)
		dstPrefix = opts.Bridge
	} else {
		srcName = "dh-" + r.EndpointID[:12]
		dstPrefix = "eth"
	}

	res.InterfaceName = InterfaceName{
		SrcName:   srcName,
		DstPrefix: dstPrefix,
	}
	log.WithFields(logFields).WithFields(log.Fields{
		"src_name":   srcName,
		"dst_prefix": dstPrefix,
	}).Trace("Join: interface naming configured")

	p.mu.Lock()
	hint, ok := p.joinHints[r.EndpointID]
	if ok {
		delete(p.joinHints, r.EndpointID)
	}
	p.mu.Unlock()
	if !ok {
		return res, util.ErrNoHint
	}

	if hint.Gateway != "" {
		log.WithFields(log.Fields{
			"network":  r.NetworkID[:12],
			"endpoint": r.EndpointID[:12],
			"sandbox":  r.SandboxKey,
			"gateway":  hint.Gateway,
		}).Info("[Join] Setting IPv4 gateway retrieved from initial DHCP in CreateEndpoint")
		res.Gateway = hint.Gateway
	}

	bridge, err := netlink.LinkByName(opts.Bridge)
	if err != nil {
		return res, fmt.Errorf("failed to get bridge interface: %w", err)
	}

	if err := p.addRoutes(&opts, false, bridge, r, hint, &res); err != nil {
		return res, err
	}
	if opts.IPv6 {
		if err := p.addRoutes(&opts, true, bridge, r, hint, &res); err != nil {
			return res, err
		}
	}

	// Persist a partial state entry immediately — before the goroutine starts —
	// so a plugin crash between Join HTTP 200 and goroutine completion does not
	// lose the endpoint. Hostname is not known yet; the goroutine updates it.
	p.addStateEntry(endpointState{
		NetworkID:  r.NetworkID,
		EndpointID: r.EndpointID,
		SandboxKey: r.SandboxKey,
		Mode:       opts.NetMode(),
		Bridge:     opts.Bridge,
		IPv6:       opts.IPv6,
		IfIndex:    hint.IfIndex,
	})

	go func() {
		defer func() {
			if recoverPanic := recover(); recoverPanic != nil {
				log.WithFields(logFields).WithFields(log.Fields{
					"panic":     fmt.Sprintf("%v", recoverPanic),
					"goroutine": "join-dhcp-manager",
				}).Error("CRITICAL: Join goroutine panicked — recovered from panic")
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), p.awaitTimeout)
		p.mu.Lock()
		p.cancelJoin[r.EndpointID] = cancel
		p.mu.Unlock()
		defer cancel()
		defer func() {
			p.mu.Lock()
			delete(p.cancelJoin, r.EndpointID)
			p.mu.Unlock()
		}()

		m := newDHCPManager(p.docker, r, opts)
		m.LastIP = hint.IPv4
		m.LastIPv6 = hint.IPv6
		m.IfIndex = hint.IfIndex

		if err := m.Start(ctx); err != nil {
			log.WithError(err).WithFields(logFields).Error("Failed to start persistent DHCP client")
			return
		}

		p.mu.Lock()
		// Check if Leave was called while we were starting.
		if _, left := p.leftEndpoints[r.EndpointID]; left {
			delete(p.leftEndpoints, r.EndpointID)
			p.mu.Unlock()
			_ = m.Stop()
			log.WithFields(logFields).Debug("Join completed but endpoint was left during startup; cleaned up manager")
			return
		}
		p.persistentDHCP[r.EndpointID] = m
		p.mu.Unlock()

		// Update hostname in the persisted entry now that Start() resolved it.
		if m.hostname != "" {
			p.updateStateEntry(r.EndpointID, func(e *endpointState) {
				e.Hostname = m.hostname
			})
		}
		log.WithFields(logFields).WithField("hostname", m.hostname).Debug("Join: DHCP manager registered and state persisted")
	}()

	duration := time.Since(startTime)
	log.WithFields(logFields).WithFields(log.Fields{
		"static_routes": len(res.StaticRoutes),
		"ipv4_gateway":  res.Gateway,
		"ipv6_gateway":  res.GatewayIPv6,
		"duration_ms":   duration.Milliseconds(),
	}).Info("Join: completed, DHCP client starting asynchronously")

	return res, nil
}

// Leave stops the persistent DHCP client for an endpoint. If the plugin has no
// state for this endpoint (e.g. the daemon restarted between Join and Leave and
// restoration didn't pick it up), Leave is a no-op rather than an error — that
// keeps Docker from tearing down a sandbox that's otherwise healthy.
func (p *Plugin) Leave(ctx context.Context, r LeaveRequest) error {
	startTime := time.Now()
	logFields := log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
	}
	log.WithFields(logFields).Debug("Leave: starting")

	// Always drop the persisted record on Leave, regardless of whether we
	// still have an in-memory manager. Otherwise a stale entry would survive
	// a plugin restart and cause a phantom Restore for a long-gone endpoint.
	defer p.removeStateEntry(r.EndpointID)

	p.mu.Lock()
	manager, ok := p.persistentDHCP[r.EndpointID]
	if ok {
		delete(p.persistentDHCP, r.EndpointID)
		p.mu.Unlock()

		log.WithFields(logFields).Debug("Leave: stopping persistent DHCP client")
		if err := manager.Stop(); err != nil {
			log.WithFields(logFields).WithError(err).Error("Leave: failed to stop DHCP client")
			return err
		}
		duration := time.Since(startTime)
		log.WithFields(logFields).WithField("duration_ms", duration.Milliseconds()).Info("Leave: completed successfully")
		return nil
	}

	// No active manager: Join goroutine may be in-flight — cancel it and mark as left.
	if cancel, pending := p.cancelJoin[r.EndpointID]; pending {
		cancel()
		delete(p.cancelJoin, r.EndpointID)
		log.WithFields(logFields).Debug("Leave: cancelled in-flight Join goroutine")
	}
	p.leftEndpoints[r.EndpointID] = struct{}{}
	p.mu.Unlock()

	log.WithFields(logFields).Debug("Leave: no active DHCP manager (Join may still be starting)")
	return nil
}
