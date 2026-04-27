package plugin

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	docker "github.com/moby/moby/client"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

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
	stopOnce  sync.Once
	started   bool
	errChan   chan error
	errChanV6 chan error
}

func newDHCPManager(docker *docker.Client, r JoinRequest, opts DHCPNetworkOptions) *dhcpManager {
	return &dhcpManager{
		docker:   docker,
		joinReq:  r,
		opts:     opts,
		stopChan: make(chan struct{}),
	}
}

func (m *dhcpManager) logFields(v6 bool) log.Fields {
	return log.Fields{
		"network":  shortID(m.joinReq.NetworkID),
		"endpoint": shortID(m.joinReq.EndpointID),
		"sandbox":  m.joinReq.SandboxKey,
		"is_ipv6":  v6,
	}
}

func (m *dhcpManager) closeStopChan() {
	m.stopOnce.Do(func() { close(m.stopChan) })
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
			log.
				WithError(err).
				WithFields(m.logFields(v6)).
				WithField("new_ip", ip).
				Error("failed to add new IP — attempting to restore previous")
			if rerr := m.netHandle.AddrAdd(m.ctrLink, lastIP); rerr != nil {
				log.
					WithError(rerr).
					WithFields(m.logFields(v6)).
					WithField("ip", lastIP).
					Error("failed to restore previous IP — container has no IP address")
			}
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

		routes, err := m.netHandle.RouteListFiltered(netlinkFamilyV4, &netlink.Route{
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

	restartBackoffs := []time.Duration{1 * time.Second, 5 * time.Second, 15 * time.Second}

	errChan := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.WithFields(m.logFields(v6)).WithField("panic", fmt.Sprintf("%v", r)).Error("CRITICAL: setupClient event loop panicked")
				errChan <- fmt.Errorf("setupClient event loop panic: %v", r)
			}
		}()

		// activeClient/activeEvents are reassigned on restart; kept local to
		// this goroutine so there is no shared-state race.
		activeClient := client
		activeEvents := events

		for {
			select {
			case event, ok := <-activeEvents:
				if !ok {
					log.WithFields(m.logFields(v6)).Error("udhcpc process exited unexpectedly — attempting restart")
					activeClient.Drain() // reap zombie before restarting

					restarted := false
					for attempt, delay := range restartBackoffs {
						select {
						case <-time.After(delay):
						case <-m.stopChan:
							errChan <- nil
							return
						}
						newClient, err := udhcpc.NewDHCPClient(m.ctrLink.Attrs().Name, &udhcpc.DHCPClientOptions{
							Hostname:  m.hostname,
							V6:        v6,
							Namespace: m.nsPath,
						})
						if err != nil {
							log.WithError(err).WithFields(m.logFields(v6)).
								Errorf("udhcpc restart attempt %d/%d: failed to create client", attempt+1, len(restartBackoffs))
							continue
						}
						newEvents, err := newClient.Start()
						if err != nil {
							log.WithError(err).WithFields(m.logFields(v6)).
								Errorf("udhcpc restart attempt %d/%d: failed to start", attempt+1, len(restartBackoffs))
							continue
						}
						activeClient = newClient
						activeEvents = newEvents
						// Reset lease state — new process must rediscover from scratch.
						if v6 {
							m.primedV6 = false
							m.deconfiguredV6 = false
						} else {
							m.primed = false
							m.deconfigured = false
						}
						restarted = true
						log.WithFields(m.logFields(v6)).Infof("udhcpc restarted successfully (attempt %d/%d)", attempt+1, len(restartBackoffs))
						break
					}
					if !restarted {
						log.WithFields(m.logFields(v6)).Error("udhcpc restart exhausted all attempts — DHCP renewal permanently lost")
						errChan <- fmt.Errorf("udhcpc%s: all restart attempts exhausted", map[bool]string{true: "6"}[v6])
						return
					}
					continue
				}

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

				errChan <- activeClient.Finish(ctx)
				return
			}
		}
	}()

	return errChan, nil
}

// fetchHostname tries to find the container hostname via Docker API.
// Non-critical: udhcpc works without hostname; failure returns "".
func (m *dhcpManager) fetchHostname(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var ctrID string
	if err := util.AwaitCondition(ctx, func() (bool, error) {
		net, err := m.docker.NetworkInspect(ctx, m.joinReq.NetworkID, docker.NetworkInspectOptions{})
		if err != nil {
			return false, nil // best-effort, do not fail Start()
		}
		for id, info := range net.Network.Containers {
			if info.EndpointID == m.joinReq.EndpointID {
				ctrID = id
				break
			}
		}
		return ctrID != "" && !strings.HasPrefix(ctrID, "ep-"), nil
	}, pollTime); err != nil || ctrID == "" {
		log.WithFields(m.logFields(false)).Debug("fetchHostname: container not ready yet, proceeding without hostname")
		return ""
	}

	ctr, err := util.AwaitContainerInspect(ctx, m.docker, ctrID, pollTime)
	if err != nil {
		log.WithFields(m.logFields(false)).Debug("fetchHostname: inspect failed, proceeding without hostname")
		return ""
	}
	return ctr.Container.Config.Hostname
}

func (m *dhcpManager) Start(ctx context.Context) error {
	startTime := time.Now()
	log.WithFields(m.logFields(false)).Info("DHCP manager starting")

	// Use SandboxKey directly — the libnetwork standard approach.
	// This avoids /proc/<PID>/ns/net which requires pidhost and CAP_SYS_PTRACE.
	m.nsPath = m.joinReq.SandboxKey

	// Fetch hostname best-effort before opening the netns (for udhcpc -x hostname).
	m.hostname = m.fetchHostname(ctx)
	log.WithFields(m.logFields(false)).WithField("hostname", m.hostname).Debug("Container hostname resolved")

	var err error
	m.nsHandle, err = util.AwaitNetNS(ctx, m.nsPath, pollTime)
	if err != nil {
		return fmt.Errorf("failed to await container netns %s: %w", m.nsPath, err)
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
			if ctrIndex == 0 {
				return fmt.Errorf("veth peer index is 0 — peer may be destroyed")
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

	duration := time.Since(startTime)
	log.WithFields(m.logFields(false)).WithField("duration_ms", duration.Milliseconds()).Info("DHCP manager started successfully")
	return nil
}

// startClients launches the persistent udhcpc(6) clients. The caller is
// responsible for having populated m.ctrLink, m.nsHandle, m.netHandle,
// m.hostname and m.nsPath before invoking this. On error, m.stopChan is
// closed and the caller should release the netns/netlink handles.
func (m *dhcpManager) startClients() error {
	var err error
	if m.errChan, err = m.setupClient(false); err != nil {
		m.closeStopChan()
		return err
	}
	if m.opts.IPv6 {
		if m.errChanV6, err = m.setupClient(true); err != nil {
			m.closeStopChan()
			return err
		}
	}
	m.started = true
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
	if !m.started {
		return nil
	}

	defer m.nsHandle.Close()
	defer m.netHandle.Delete()

	m.closeStopChan()

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
