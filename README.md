# Docker Network DHCP Plugin

A Docker network driver plugin that allocates IP addresses via DHCP to containers on a host bridge interface or physical NIC. When configured correctly, this allows you to spin up containers and access them on your network as if they were regular machines — perfect for home lab deployments, IoT environments, and scenarios where you need containers to be discoverable on your local network!

## Features

- **DHCP IP Allocation**: Containers receive IP addresses from your network's DHCP server (e.g., router, dedicated server)
- **Multiple Network Modes**: Support for bridge, macvlan, and ipvlan networking
  - **Bridge mode**: Containers behind a host bridge with full network isolation control
  - **Macvlan mode**: Direct attachment to physical NIC with unique MAC addresses per container
  - **Ipvlan mode**: Direct attachment with shared MAC address for restricted environments
- **IPv4 & IPv6 Support**: Dual-stack networking with independent DHCP clients per protocol
- **Persistent State**: Endpoint state survives plugin restart via atomic JSON snapshots
- **Soft Lease Handover**: Maintains IP address during renewal to avoid disrupting traffic
- **Structured Logging**: Comprehensive debug logging with logrus for troubleshooting
- **UDP Broadcast/Multicast Support**: Full multicast support in macvlan/ipvlan modes
- **Production Ready**: Used in home labs, IoT deployments, and edge environments

## Requirements

- **Docker Engine**: 29.0.0 or later
- **Go**: 1.26.2+ (only for building from source)
- **DHCP Server**: Accessible from the network interface (router DHCP, Dnsmasq, ISC DHCP, etc.)
- **Linux Kernel**: netlink and network namespace support (standard on modern Linux)
- **Privileges**: Plugin requires CAP_NET_ADMIN and CAP_SYS_ADMIN capabilities

## Socket Configuration

The plugin automatically discovers the correct Unix socket path to bind to. See [SOCKET_DISCOVERY.md](SOCKET_DISCOVERY.md) for detailed information on:

- How the plugin discovers its socket path
- Why auto-discovery is needed
- How to override socket path if needed
- Deployment scenarios and troubleshooting

**TL;DR**: When installed via Docker plugin manager, the socket path is auto-discovered. No manual configuration needed.

## Installation

### From Docker Registry

The plugin is available as a Docker plugin package:

```bash
docker plugin install docker-network-dhcp:latest
```

When prompted for permissions, grant access to network and host PID namespace:

```
Plugin "docker-network-dhcp:latest" is requesting the following privileges:
 - network: [host]
 - host pid namespace: [true]
 - mount: [/var/run/docker.sock]
 - capabilities: [CAP_NET_ADMIN CAP_SYS_ADMIN CAP_SYS_PTRACE]
Do you grant the above permissions? [y/N] y
```

Verify installation:

```bash
docker plugin ls
# Should show docker-network-dhcp enabled
```

### Building from Source

Clone the repository and build:

```bash
git clone https://github.com/fernandodelucca/docker-network-dhcp.git
cd docker-network-dhcp

# Build for Linux
go build -o bin/ ./cmd/...

# Run the plugin
./bin/net-dhcp --log debug --sock /run/docker/plugins/net-dhcp.sock
```

For Docker plugin container deployment:

```bash
docker build -t docker-network-dhcp:latest .
docker plugin create docker-network-dhcp docker-network-dhcp:latest
docker plugin enable docker-network-dhcp
```

## Network Modes

Choose the mode that best fits your use case:

### Bridge Mode (Default)

**Best for**: Containers that need to communicate with the host, iptables control, or traditional bridge networking.

**Host setup required**: Create and configure a bridge on the host.

**Container interface**: Named `<bridge>0` (e.g., `mybr0` if bridge is `mybr`)

**Key characteristics**:
- Host ↔ Container communication: ✅ Supported
- UDP broadcast/multicast: Requires iptables configuration
- Per-container MAC: Not unique (inherited from bridge)
- Network isolation: Good (controlled by iptables)

**Setup example** (Ubuntu/Debian):

```bash
# Create bridge interface
sudo ip link add my-bridge type bridge
sudo ip link set my-bridge up

# Attach physical NIC to bridge (adjust eth0 as needed)
sudo ip link set eth0 up
sudo ip link set eth0 master my-bridge

# Configure firewall (if needed)
sudo iptables -A FORWARD -i my-bridge -j ACCEPT
sudo iptables -A FORWARD -o my-bridge -j ACCEPT

# Get IP from DHCP for host
sudo dhcpcd my-bridge
```

**Create network**:

```bash
docker network create \
  --driver docker-network-dhcp \
  --ipam-driver null \
  -o bridge=my-bridge \
  -o mode=bridge \
  my-dhcp-net
```

**With IPv6**:

```bash
docker network create \
  --driver docker-network-dhcp \
  --ipam-driver null \
  -o bridge=my-bridge \
  -o mode=bridge \
  -o ipv6=true \
  my-dhcp-net-v6
```

### Macvlan Mode

**Best for**: Containers that appear as independent hosts on the LAN (UDP broadcast, multicast, game servers, mDNS, discovery protocols).

**Host setup required**: None! Just point at your physical NIC.

**Container interface**: Named `eth0`

**Key characteristics**:
- Host ↔ Container communication: ❌ Not directly (Linux L2 limitation)
- UDP broadcast/multicast: ✅ Full support
- Per-container MAC: ✅ Unique MAC per container
- Network isolation: Excellent (L2 isolation)
- DHCP reservations: ✅ Work by MAC address

**Create network**:

```bash
docker network create \
  --driver docker-network-dhcp \
  --ipam-driver null \
  -o bridge=eth0 \
  -o mode=macvlan \
  my-macvlan-dhcp
```

**With IPv6**:

```bash
docker network create \
  --driver docker-network-dhcp \
  --ipam-driver null \
  -o bridge=eth0 \
  -o mode=macvlan \
  -o ipv6=true \
  my-macvlan-dhcp-v6
```

**Host ↔ Container workaround** (if needed):

Create a dedicated macvlan interface on the host:

```bash
sudo ip link add eth0.host link eth0 type macvlan mode bridge
sudo ip link set eth0.host up
sudo dhcpcd eth0.host
```

Now the host can reach containers on the macvlan network.

### Ipvlan Mode

**Best for**: Restricted environments where MAC spoofing is prevented by NIC or upstream switch.

**Host setup required**: None.

**Container interface**: Named `eth0`

**Key characteristics**:
- Host ↔ Container communication: ❌ Not directly
- UDP broadcast/multicast: ✅ Full support
- Per-container MAC: ❌ All containers share parent NIC's MAC
- Network isolation: Good
- DHCP reservations: ⚠️ Requires manual MAC-based client IDs (not recommended)

**⚠️ Important**: All containers share the parent NIC's MAC address, which causes DHCP servers that key leases to MAC to assign the same IP to all containers. Use only if MAC spoofing restrictions force you to.

**Create network**:

```bash
docker network create \
  --driver docker-network-dhcp \
  --ipam-driver null \
  -o bridge=eth0 \
  -o mode=ipvlan \
  my-ipvlan-dhcp
```

## Configuration

### Network Options

| Option | Type | Required | Default | Description |
|--------|------|----------|---------|-------------|
| `bridge` | string | ✅ | — | Bridge interface (bridge mode) or physical NIC (macvlan/ipvlan) |
| `mode` | string | ❌ | bridge | Network mode: `bridge`, `macvlan`, or `ipvlan` |
| `ipv6` | bool | ❌ | false | Enable IPv6 DHCP on the network |
| `lease_timeout` | duration | ❌ | 10s | Max time to wait for initial DHCP lease (e.g., "30s", "1m") |
| `ignore_conflicts` | bool | ❌ | false | Allow bridge to have pre-existing IP configuration (bridge mode only) |
| `skip_routes` | bool | ❌ | false | Don't copy routes from DHCP server to container |

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_LEVEL` | info | Log level: trace, debug, info, warn, error |
| `LOGFILE` | — | Log file path (empty = stdout) |
| `AWAIT_TIMEOUT` | 10s | Timeout waiting for container namespace preparation |

## Usage Examples

### Basic Container

```bash
docker run --rm -ti --network my-dhcp-net alpine sh

# Inside container:
/ # ip addr show
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 ...
    inet 127.0.0.1/8 ...
2: my-bridge0@if160: <BROADCAST,MULTICAST,UP,LOWER_UP,M-DOWN> mtu 1500 ...
    inet 192.168.1.100/24 brd 192.168.1.255 scope global
    inet6 fe80::4242:c0ff:fea8:132/64 scope link

/ # ip route show
default via 192.168.1.1 dev my-bridge0

/ # ping -c 1 192.168.1.1
PING 192.168.1.1 (192.168.1.1): 56 data bytes
64 bytes from 192.168.1.1: seq=0 ttl=64 time=1.234 ms
```

### With Reserved IP (via MAC address)

```bash
# Configure DHCP server to reserve 192.168.1.50 for MAC 86:41:68:f8:85:b9
docker run -d \
  --network my-dhcp-net \
  --mac-address 86:41:68:f8:85:b9 \
  --hostname my-app \
  nginx
```

### Docker Compose

```yaml
version: '3.8'

services:
  web:
    image: nginx
    hostname: my-web-server
    mac_address: 86:41:68:f8:85:b9  # Optional: for DHCP reservation
    networks:
      - lan

  app:
    image: myapp:latest
    hostname: my-app-server
    environment:
      - DATABASE_HOST=my-db-server  # DNS discovery
    networks:
      - lan
    depends_on:
      - db

  db:
    image: postgres:latest
    hostname: my-db-server
    networks:
      - lan

networks:
  lan:
    driver: docker-network-dhcp
    driver_opts:
      bridge: eth0
      mode: macvlan           # or 'bridge', 'ipvlan'
      ipv6: 'true'           # Optional IPv6 support
      lease_timeout: '30s'    # Increase if slow DHCP server
      skip_routes: 'false'    # Copy DHCP routes to container
    ipam:
      driver: 'null'          # Important: use null IPAM driver
```

Save as `docker-compose.yml` and run:

```bash
docker-compose up -d
```

## How It Works

### Bridge Mode Flow

1. **Network creation**: Validates bridge interface and IPAM settings
2. **Container create**: veth pair created, host end attached to bridge
3. **DHCP discover**: udhcpc starts on container end (in host namespace) to get initial IP
4. **Container move**: Docker moves veth into container namespace
5. **Persistent lease**: Plugin starts udhcpc in container namespace for lease renewal
6. **Lease renewal**: udhcpc renews lease when needed, plugin applies updates

### Macvlan/Ipvlan Mode Flow

1. **Network creation**: Validates parent physical NIC
2. **Container create**: macvlan (or ipvlan) sub-interface created on parent NIC
3. **DHCP discover**: udhcpc starts on sub-interface to get initial IP
4. **Namespace move**: Docker moves sub-interface into container namespace
5. **Persistent lease**: Plugin starts udhcpc in container namespace for renewal
6. **Automatic cleanup**: When container exits, namespace is destroyed and sub-interface removed automatically

## Important Notes

### General

- **Startup time**: Containers take longer to start (DHCP negotiation adds latency)
- **Lease renewal**: Persistent DHCP client renews automatically and updates routes as needed
- **Soft handover**: IP address is maintained during renewal to avoid traffic disruption
- **State persistence**: Plugin state is saved to disk and restored on restart
- **Null IPAM driver**: ⚠️ **Must** use `--ipam-driver null` or Docker will allocate conflicting IPs

### Performance Tuning

If containers take too long to start:

```bash
# Increase lease timeout (default 10s)
docker network create \
  --driver docker-network-dhcp \
  --ipam-driver null \
  -o bridge=eth0 \
  -o mode=macvlan \
  -o lease_timeout=60s \
  my-dhcp-net
```

### Bridge Mode Conflicts

By default, the plugin checks that a bridge is only used by one DHCP network:

```bash
# To disable this check (if mistakenly detecting conflicts)
docker network create \
  --driver docker-network-dhcp \
  --ipam-driver null \
  -o bridge=my-bridge \
  -o ignore_conflicts=true \
  my-dhcp-net
```

### Route Handling

The plugin copies static routes from DHCP server to containers. To disable:

```bash
docker network create \
  --driver docker-network-dhcp \
  --ipam-driver null \
  -o bridge=eth0 \
  -o skip_routes=true \
  my-dhcp-net
```

## Debugging & Troubleshooting

### Check Plugin Status

```bash
# List plugins
docker plugin ls

# Inspect plugin details
docker plugin inspect docker-network-dhcp

# Test socket responsiveness
./scripts/health-check.sh

# Or manually test
curl -s --unix-socket /run/docker/plugins/net-dhcp.sock \
  -X POST http://localhost/NetworkDriver.GetCapabilities \
  -H 'Content-Type: application/json' \
  -d '{}' | jq .
```

### View Plugin Logs

```bash
# For plugin running as container
docker logs <plugin-container-id>

# For plugin running on host
tail -f /var/lib/docker/plugins/*/rootfs/var/log/net-dhcp.log

# Enable debug logging
docker plugin set docker-network-dhcp LOG_LEVEL=trace
docker plugin disable docker-network-dhcp
docker plugin enable docker-network-dhcp
```

### Common Issues

**Plugin shows as disabled**:
```bash
# Check logs first
docker logs <container> | grep -i error

# Restart plugin
docker plugin disable docker-network-dhcp
docker plugin enable docker-network-dhcp

# Verify socket is listening
docker exec <some-container> curl http://172.17.0.1:8080/health 2>/dev/null || echo "Not reachable"
```

**Containers not getting IP**:
```bash
# Verify DHCP server is running
ps aux | grep dhcp

# Check bridge has connectivity to DHCP server
ping <dhcp-server-ip>

# Increase lease timeout
docker network create \
  --driver docker-network-dhcp \
  --ipam-driver null \
  -o bridge=eth0 \
  -o mode=macvlan \
  -o lease_timeout=60s \
  my-dhcp-net

# Check container logs
docker logs <container-id>
```

**Container IP changes unexpectedly**:
- DHCP lease expired before renewal could happen
- Increase `lease_timeout` in network options
- Check DHCP server configuration for lease time

**High CPU/memory usage**:
- Check for orphaned udhcpc processes: `ps aux | grep udhcpc`
- Review endpoint count: `cat /var/lib/net-dhcp/state.json | jq '. | length'`
- Check logs for panic recovery messages

## Security

See [SECURITY.md](SECURITY.md) for:
- Unix socket permission model and security implications
- Information disclosure prevention
- Error handling best practices
- Deployment recommendations (container vs host)
- Recommended monitoring and alerting

## Performance Characteristics

| Metric | Value | Notes |
|--------|-------|-------|
| **Initial lease time** | ~1-3s | Depends on DHCP server response |
| **Container startup overhead** | +2-5s | DHCP negotiation adds latency |
| **Lease renewal frequency** | Network dependent | Typically every 30min-12h |
| **Memory per endpoint** | ~5-10 MB | Includes DHCP client state |
| **CPU during renewal** | Minimal | Only during lease renewal |
| **Max endpoints tested** | 100+ | Atomic persistence at scale |

## Architecture Overview

```
┌─────────────────────────────────────────────────────────┐
│  Docker Daemon                                          │
│  ┌───────────────────────────────────────────────────┐  │
│  │ libnetwork (Network Manager)                      │  │
│  │ ┌──────────────────────────────────────────────┐  │  │
│  │ │ net-dhcp Remote Driver Plugin                │  │  │
│  │ │ HTTP Server @ /run/docker/plugins/net-dhcp.sock
│  │ │                                              │  │  │
│  │ │ ┌────────────────────────────────────────┐  │  │  │
│  │ │ │ EndpointOperations (Join/Leave/Create)│  │  │  │
│  │ │ │ StateManager (Persistence)              │  │  │  │
│  │ │ │ DHCPManager (Per-endpoint DHCP client)  │  │  │  │
│  │ │ │ RestoreManager (State recovery)          │  │  │  │
│  │ │ └────────────────────────────────────────┘  │  │  │
│  │ └──────────────────────────────────────────────┘  │  │
│  └───────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────┘
         │
         │ Network Interface Binding
         ▼
┌─────────────────────────────────────────────────────────┐
│  Host Network Stack                                     │
│                                                         │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ Bridge Mode  │  │ Macvlan Mode │  │ Ipvlan Mode  │  │
│  │              │  │              │  │              │  │
│  │ Bridge +     │  │ Sub-IF on    │  │ Sub-IF on    │  │
│  │ Veth pairs   │  │ Physical NIC │  │ Physical NIC │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
└─────────────────────────────────────────────────────────┘
         │
         │ DHCP Traffic (UDP)
         ▼
┌─────────────────────────────────────────────────────────┐
│  DHCP Server (Router, Dnsmasq, ISC DHCP, etc.)         │
└─────────────────────────────────────────────────────────┘
```

## References & Documentation

- **[SECURITY.md](SECURITY.md)** - Security model, permission management, best practices
- **[Docker Network Plugins](https://docs.docker.com/engine/extend/plugins_network/)** - Official Docker plugin documentation
- **[libnetwork Remote Driver Protocol](https://github.com/moby/libnetwork/blob/master/docs/remote.md)** - Network driver specification
- **[Go Security Best Practices](https://go.dev/doc/security/best-practices)** - Go security guidelines
- **[DHCP Protocol (RFC 2131)](https://tools.ietf.org/html/rfc2131)** - DHCP standard

## Contributing

Contributions are welcome! Please review [SECURITY.md](SECURITY.md) and ensure:
- All tests pass: `go test ./...`
- Code is formatted: `go fmt ./...`
- No issues from linter: `go vet ./...`
- Documentation is updated for new features

## License

[Specify your license here - e.g., Apache 2.0, MIT, GPL v3]

## Support

For issues, feature requests, or questions:
1. Check [SECURITY.md](SECURITY.md) for security guidelines
2. Review the [Troubleshooting](#debugging--troubleshooting) section
3. Check existing GitHub issues
4. Enable debug logging and include logs with any issue report
