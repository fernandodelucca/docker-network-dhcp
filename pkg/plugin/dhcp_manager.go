package plugin

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	dNetwork "github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/fernandodelucca/docker-network-dhcp/pkg/udhcpc"
	"github.com/fernandodelucca/docker-network-dhcp/pkg/util"
)

const pollTime = 100 * time.Millisecond

type dhcpManager struct {
	docker  *docker.Client
	joinReq JoinRequest
	opts    DHCPNetworkOptions

	LastIP   *netlink.Addr
	LastIPv6 *netlink.Addr
	// IfIndex is the pre-move interface index for macvlan/ipvlan endpoints.
	// The ifindex is stable across namespace moves, so it can be used to locate
	// the interface after Docker moves it into the container namespace.
	IfIndex int

	nsPath    string
	hostname  string
	nsHandle  netns.NsHandle
	netHandle *netlink.Handle
	ctrLink   netlink.Link

	// deconfigured is set when udhcpc emits a deconfig event after the lease
	// was already in place. We keep the existing IP ("soft handover") and apply
	// the next bound/renew lease as a replacement to avoid breaking copied routes.
	// primed becomes true on the first bound/renew so that the deconfig udhcpc
	// always emits at startup (before DHCPDISCOVER) is ignored — at that point
	// the IP set by CreateEndpoint is the source of truth, not a stale lease.
	primed         bool
	primedV6       bool
	deconfigured   bool
	deconfiguredV6 bool

	stopChan  chan struct{}
	errChan   chan error
	errChanV6 chan error
}

func newDHCPManager(docker *docker.Client, r JoinRequest, opts DHCPNetworkOptions) *dhcpManager {
	return &dhcpManager{
		docker:  docker,
		joinReq: r,
		opts:    opts,

		stopChan: make(chan struct{}),
	}
}

func (m *dhcpManager) logFields(v6 bool) log.Fields {
	return log.Fields{
		"network":  m.joinReq.NetworkID[:12],
		"endpoint": m.joinReq.EndpointID[:12],
		"sandbox":  m.joinReq.SandboxKey,
		"is_ipv6":  v6,
	}
}

func (m *dhcpManager) renew(v6 bool, info udhcpc.Info) error {
	lastIP := m.LastIP
	if v6 {
		lastIP = m.LastIPv6
	}

	ip, err := netlink.ParseAddr(info.IP)
	if err != nil {
		return fmt.Errorf("failed to parse IP address: %w", err)
	}

	deconfigured := m.deconfigured
	if v6 {
		deconfigured = m.deconfiguredV6
	}

	// Replace the IP on the container interface when:
	//  - the lease IP differs from what we previously installed, or
	//  - we previously saw a deconfig event and deferred deletion (soft handover).
	// Note: docker inspect's view of the endpoint address won't refresh — libnetwork
	// has no API to mutate an endpoint's address outside Leave/Join. The container
	// itself sees the new IP and traffic flows; the inspect output is a known limitation.
	if !ip.Equal(*lastIP) || deconfigured {
		log.
			WithFields(m.logFields(v6)).
			WithField("old_ip", lastIP).
			WithField("new_ip", ip).
			WithField("deconfigured", deconfigured).
			Info("udhcpc lease replacing IP on container interface")

		if err := m.netHandle.AddrDel(m.ctrLink, lastIP); err != nil {
			log.
				WithError(err).
				WithFields(m.logFields(v6)).
				WithField("ip", lastIP).
				Debug("failed to delete previous IP (best-effort)")
		}
		if err := m.netHandle.AddrAdd(m.ctrLink, ip); err != nil {
			return fmt.Errorf("failed to add IP address: %w", err)
		}

		if v6 {
			m.LastIPv6 = ip
			m.deconfiguredV6 = false
		} else {
			m.LastIP = ip
			m.deconfigured = false
		}
	}

	if !v6 && info.Gateway != "" {
		newGateway := net.ParseIP(info.Gateway)

		routes, err := m.netHandle.RouteListFiltered(unix.AF_INET, &netlink.Route{
			LinkIndex: m.ctrLink.Attrs().Index,
			Dst:       nil,
		}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_DST)
		if err != nil {
			return fmt.Errorf("failed to list routes: %w", err)
		}

		if len(routes) == 0 {
			log.
				WithFields(m.logFields(v6)).
				WithField("gateway", newGateway).
				Info("udhcpc renew adding default route")

			if err := m.netHandle.RouteAdd(&netlink.Route{
				LinkIndex: m.ctrLink.Attrs().Index,
				Gw:        newGateway,
			}); err != nil {
				return fmt.Errorf("failed to add default route: %w", err)
			}
		} else if !newGateway.Equal(routes[0].Gw) {
			log.
				WithFields(m.logFields(v6)).
				WithField("old_gateway", routes[0].Gw).
				WithField("new_gateway", newGateway).
				Info("udhcpc renew replacing default route")

			routes[0].Gw = newGateway
			if err := m.netHandle.RouteReplace(&routes[0]); err != nil {
				return fmt.Errorf("failed to replace default route: %w", err)
			}
		}
	}

	return nil
}

func (m *dhcpManager) setupClient(v6 bool) (chan error, error) {
	v6Str := ""
	if v6 {
		v6Str = "v6"
	}

	log.
		WithFields(m.logFields(v6)).
		Info("Starting persistent DHCP client")

	client, err := udhcpc.NewDHCPClient(m.ctrLink.Attrs().Name, &udhcpc.DHCPClientOptions{
		Hostname:  m.hostname,
		V6:        v6,
		Namespace: m.nsPath,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create DHCP%v client: %w", v6Str, err)
	}

	events, err := client.Start()
	if err != nil {
		return nil, fmt.Errorf("failed to start DHCP%v client: %w", v6Str, err)
	}

	errChan := make(chan error)
	go func() {
		for {
			select {
			case event := <-events:
				switch event.Type {
				case "deconfig":
					// udhcpc always emits deconfig once at startup (before
					// DHCPDISCOVER). At that point we haven't seen bound yet —
					// primed is false — and the current IP is what CreateEndpoint
					// installed, not a stale lease. Skip the soft-handover dance
					// in that case to avoid a no-op AddrReplace at boot.
					primed := m.primed
					if v6 {
						primed = m.primedV6
					}
					if !primed {
						log.WithFields(m.logFields(v6)).Debug("udhcpc startup deconfig — ignoring (not yet primed)")
						continue
					}

					// Soft handover after we've been primed: don't delete the
					// IP yet (would drop routes copied from the host bridge).
					// Mark deconfigured so the next bound/renew does AddrReplace.
					log.
						WithFields(m.logFields(v6)).
						Info("udhcpc deconfig — deferring IP removal until next lease (soft handover)")
					if v6 {
						m.deconfiguredV6 = true
					} else {
						m.deconfigured = true
					}
				case "bound":
					// "bound" fires on the first lease (after CreateEndpoint already
					// set the IP) or after a deconfig/lease loss. Mark primed so
					// future deconfigs are taken seriously, then treat it as renew.
					log.WithFields(m.logFields(v6)).Debug("udhcpc bound")
					if v6 {
						m.primedV6 = true
					} else {
						m.primed = true
					}
					if err := m.renew(v6, event.Data); err != nil {
						log.
							WithError(err).
							WithFields(m.logFields(v6)).
							WithField("gateway", event.Data.Gateway).
							WithField("new_ip", event.Data.IP).
							Error("Failed to apply bound lease")
					}
				case "renew":
					log.
						WithFields(m.logFields(v6)).
						Debug("udhcpc renew")
					if v6 {
						m.primedV6 = true
					} else {
						m.primed = true
					}

					if err := m.renew(v6, event.Data); err != nil {
						log.
							WithError(err).
							WithFields(m.logFields(v6)).
							WithField("gateway", event.Data.Gateway).
							WithField("new_ip", event.Data.IP).
							Error("Failed to execute IP renewal")
					}
				case "leasefail":
					log.WithFields(m.logFields(v6)).Warn("udhcpc failed to get a lease")
				case "nak":
					log.WithFields(m.logFields(v6)).Warn("udhcpc client received NAK")
				}

			case <-m.stopChan:
				log.
					WithFields(m.logFields(v6)).
					Info("Shutting down persistent DHCP client")

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				errChan <- client.Finish(ctx)
				return
			}
		}
	}()

	return errChan, nil
}

func (m *dhcpManager) Start(ctx context.Context) error {
	var ctrID string
	if err := util.AwaitCondition(ctx, func() (bool, error) {
		dockerNet, err := m.docker.NetworkInspect(ctx, m.joinReq.NetworkID, dNetwork.InspectOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to get Docker network info: %w", err)
		}

		for id, info := range dockerNet.Containers {
			if info.EndpointID == m.joinReq.EndpointID {
				ctrID = id
				break
			}
		}
		if ctrID == "" {
			return false, util.ErrNoContainer
		}

		// Seems like Docker makes the container ID just the endpoint until it's ready
		return !strings.HasPrefix(ctrID, "ep-"), nil
	}, pollTime); err != nil {
		return err
	}

	ctr, err := util.AwaitContainerInspect(ctx, m.docker, ctrID, pollTime)
	if err != nil {
		return fmt.Errorf("failed to get Docker container info: %w", err)
	}

	// Using the "sandbox key" directly causes issues on some platforms
	m.nsPath = fmt.Sprintf("/proc/%v/ns/net", ctr.State.Pid)
	m.hostname = ctr.Config.Hostname

	m.nsHandle, err = util.AwaitNetNS(ctx, m.nsPath, pollTime)
	if err != nil {
		return fmt.Errorf("failed to get sandbox network namespace: %w", err)
	}

	m.netHandle, err = netlink.NewHandleAt(m.nsHandle)
	if err != nil {
		m.nsHandle.Close()
		return fmt.Errorf("failed to open netlink handle in sandbox namespace: %w", err)
	}

	if err := func() error {
		var ctrIndex int
		var oldCtrName string

		if m.opts.NetMode() == NetworkModeBridge {
			hostName, vethCtrName := vethPairNames(m.joinReq.EndpointID)
			hostLink, err := netlink.LinkByName(hostName)
			if err != nil {
				return fmt.Errorf("failed to find host side of veth pair: %w", err)
			}
			hostVeth, ok := hostLink.(*netlink.Veth)
			if !ok {
				return util.ErrNotVEth
			}

			ctrIndex, err = netlink.VethPeerIndex(hostVeth)
			if err != nil {
				return fmt.Errorf("failed to get container side of veth's index: %w", err)
			}
			oldCtrName = vethCtrName
		} else {
			// macvlan/ipvlan: locate the interface by the index recorded during CreateEndpoint.
			// Linux preserves ifindex when an interface moves between namespaces.
			ctrIndex = m.IfIndex
			oldCtrName = "dh-" + m.joinReq.EndpointID[:12]
		}

		if err := util.AwaitCondition(ctx, func() (bool, error) {
			var err error
			m.ctrLink, err = util.AwaitLinkByIndex(ctx, m.netHandle, ctrIndex, pollTime)
			if err != nil {
				return false, fmt.Errorf("failed to get link for container interface: %w", err)
			}

			return m.ctrLink.Attrs().Name != oldCtrName, nil
		}, pollTime); err != nil {
			return err
		}

		return m.startClients()
	}(); err != nil {
		m.netHandle.Delete()
		m.nsHandle.Close()
		return err
	}

	return nil
}

// startClients launches the persistent udhcpc(6) clients. The caller is
// responsible for having populated m.ctrLink, m.nsHandle, m.netHandle,
// m.hostname and m.nsPath before invoking this. On error, m.stopChan is
// closed and the caller should release the netns/netlink handles.
func (m *dhcpManager) startClients() error {
	var err error
	if m.errChan, err = m.setupClient(false); err != nil {
		close(m.stopChan)
		return err
	}
	if m.opts.IPv6 {
		if m.errChanV6, err = m.setupClient(true); err != nil {
			close(m.stopChan)
			return err
		}
	}
	return nil
}

// RestoreClient launches the persistent udhcpc(6) clients for an endpoint
// whose container is already running and whose interface is already inside
// the container netns (e.g. after a plugin restart). The caller must populate
// nsPath, hostname, ctrLink, nsHandle, netHandle, LastIP/LastIPv6 and IfIndex
// before invoking this.
func (m *dhcpManager) RestoreClient(ctx context.Context) error {
	if err := m.startClients(); err != nil {
		m.netHandle.Delete()
		m.nsHandle.Close()
		return err
	}
	return nil
}

func (m *dhcpManager) Stop() error {
	defer m.nsHandle.Close()
	defer m.netHandle.Delete()

	close(m.stopChan)

	if err := <-m.errChan; err != nil {
		return fmt.Errorf("failed shut down DHCP client: %w", err)
	}
	if m.opts.IPv6 {
		if err := <-m.errChanV6; err != nil {
			return fmt.Errorf("failed shut down DHCPv6 client: %w", err)
		}
	}

	return nil
}
