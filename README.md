# Docker Network DHCP Plugin

A Docker network driver plugin that allocates IP addresses via DHCP to containers on a host bridge interface.

## Features

- **DHCP IP Allocation**: Containers get IP addresses from a DHCP server on the specified bridge
- **IPv4 & IPv6 Support**: Dual-stack networking with separate DHCP clients per protocol
- **Persistent State**: Endpoint state survives plugin restart via atomic JSON snapshots
- **Bridge Support**: Works with bridge, macvlan, and ipvlan network modes
- **Soft Handover**: Maintains IP address during lease renewal to avoid disrupting traffic
- **Structured Logging**: Comprehensive logging with logrus for debugging and monitoring

## Requirements

- Docker Engine 29.0.0+
- Go 1.26.2+ (for building from source)
- A DHCP server accessible from the bridge interface
- Linux kernel with netlink and network namespace support

## Quick Start

```bash
# Create DHCP network
docker network create -d net-dhcp \
  --opt=com.docker.network.generic='{"bridge":"eth0"}' \
  dhcp-net

# Run container
docker run -it --net=dhcp-net alpine sh

# Check IP
/ # ip addr show
```

## Configuration

### Network Options

| Option | Type | Required | Default | Description |
|--------|------|----------|---------|-------------|
| `bridge` | string | ✅ | — | Bridge interface name (e.g., eth0, br0) |
| `mode` | string | ❌ | bridge | Network mode: `bridge`, `macvlan`, `ipvlan` |
| `ipv6` | bool | ❌ | false | Enable IPv6 DHCP on the network |
| `lease_timeout` | duration | ❌ | 10s | Timeout waiting for DHCP lease (e.g., "30s", "1m") |
| `ignore_conflicts` | bool | ❌ | false | Allow bridge to have pre-existing IP configuration |
| `skip_routes` | bool | ❌ | false | Skip adding DHCP-provided routes to containers |

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_LEVEL` | info | Log level: trace, debug, info, warn, error |
| `LOGFILE` | — | Log file path (empty = stdout) |
| `AWAIT_TIMEOUT` | 10s | Timeout waiting for container inspection |

## Security

See [SECURITY.md](SECURITY.md) for security guidelines, socket permissions, and deployment best practices.

## Troubleshooting

### Plugin Marked as Disabled

Check logs and verify socket is responsive:
```bash
curl -s --unix-socket /run/docker/plugins/net-dhcp.sock \
  -X POST http://localhost/NetworkDriver.GetCapabilities \
  -H 'Content-Type: application/json' \
  -d '{}' | jq .
```

### Containers Not Getting IP

Increase lease timeout if DHCP server is slow:
```bash
docker network create -d net-dhcp \
  --opt=com.docker.network.generic='{"bridge":"eth0","lease_timeout":"30s"}' \
  dhcp-net
```

## Performance

- **Lease Timeout**: Default 10s, increase if DHCP server is slow
- **IPv6**: Doubles resource usage per endpoint (second DHCP client)
- **State Persistence**: Atomic writes, tested with 100s of endpoints

## Architecture

- **Network Driver**: HTTP server on Unix socket `/run/docker/plugins/net-dhcp.sock`
- **DHCP Client**: udhcpc runs in each container namespace for lease management
- **State Persistence**: Atomic JSON snapshots in `/var/lib/net-dhcp/state.json`
- **Restore**: Automatic re-attachment to existing containers on plugin restart

## References

- [Docker Network Plugins Documentation](https://docs.docker.com/engine/extend/plugins_network/)
- [libnetwork Remote Driver Protocol](https://github.com/moby/libnetwork/blob/master/docs/remote.md)
