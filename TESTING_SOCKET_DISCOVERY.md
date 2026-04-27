# Testing Socket Path Discovery

## Prerequisites

- Linux environment with Docker Engine 29.0+
- Docker daemon running
- Plugin source code with socket discovery changes
- Basic understanding of Docker plugin architecture

## Build and Create Plugin

### Step 1: Build the Docker Image

```bash
cd docker-network-dhcp
docker build -t docker-network-dhcp:test .
```

Expected output:
```
Step 1/11 : FROM golang:1.26.2-alpine AS builder
...
Step 11/11 : ENTRYPOINT ["/usr/sbin/net-dhcp"]
Successfully built <image_hash>
Successfully tagged docker-network-dhcp:test:latest
```

### Step 2: Create the Plugin

```bash
# Create plugin from the image
docker plugin create docker-network-dhcp docker-network-dhcp:test

# Verify plugin is created (should show as disabled initially)
docker plugin ls
# PLUGIN ID    NAME                          ENABLED DESCRIPTION
# <id>         docker-network-dhcp:latest    false   Docker Network DHCP Plugin...
```

## Test Level 3: Auto-Discovery (Docker Plugin Manager)

This is the primary deployment scenario.

### Enable Plugin

```bash
docker plugin enable docker-network-dhcp
```

Expected behavior:
1. Docker creates `/run/docker/plugins/<PLUGIN_ID>/` on the host
2. Plugin container starts
3. Plugin's `discoverSocketPath()` detects the directory
4. Plugin logs: `INFO Auto-discovered socket path auto_discovered=/run/docker/plugins/<PLUGIN_ID>/net-dhcp.sock`
5. Docker detects the socket
6. Plugin status changes to `enabled`

### Verify in Logs

```bash
# Watch plugin logs in real-time
docker plugin logs -f docker-network-dhcp

# Or check logs after enable
docker plugin logs docker-network-dhcp | grep -i socket
```

Expected log entries:
```
INFO  Plugin starting up log_level=info socket=/run/docker/plugins/<ID>/net-dhcp.sock
INFO  Auto-discovered socket path auto_discovered=/run/docker/plugins/<ID>/net-dhcp.sock
INFO  Docker daemon is ready — plugin can now handle requests
INFO  Plugin socket created and accessible socket=/run/docker/plugins/<ID>/net-dhcp.sock mode=0660 size=0
INFO  Starting server...
```

### Verify Socket Exists

```bash
# Check if socket was created at the expected location
ls -la /run/docker/plugins/*/net-dhcp.sock

# Should show something like:
# srw-rw----  root docker 0 Apr 27 10:30 /run/docker/plugins/abc123def456/net-dhcp.sock
```

### Verify Plugin Status

```bash
docker plugin ls
# PLUGIN ID    NAME                          ENABLED DESCRIPTION
# <id>         docker-network-dhcp:latest    true    Docker Network DHCP Plugin...

# Should show ENABLED = true (not false/disabled)
```

## Test Level 2: Environment Variable Override

For scenarios where auto-discovery needs to be overridden.

### Disable Current Plugin

```bash
docker plugin disable docker-network-dhcp
docker plugin rm docker-network-dhcp
```

### Recreate with Environment Variable

```bash
# Create plugin again
docker plugin create docker-network-dhcp docker-network-dhcp:test

# Set environment variable before enabling
# (This typically requires plugin config, which isn't standard)
# As a workaround, you can test locally:

# Kill existing container (if running)
pkill -f net-dhcp

# Run with explicit env var
export DOCKER_PLUGIN_SOCKET=/run/docker/plugins/test-socket-id/net-dhcp.sock
mkdir -p /run/docker/plugins/test-socket-id/
./bin/net-dhcp --log debug
```

Expected logs:
```
INFO  Socket path from environment variable source=DOCKER_PLUGIN_SOCKET
INFO  Plugin socket created and accessible socket=/run/docker/plugins/test-socket-id/net-dhcp.sock
```

## Test Level 1: Explicit Flag

For manual testing and debugging.

### Manual Test with Flag

```bash
# Kill any running plugin
pkill -f net-dhcp

# Run with explicit socket path
mkdir -p /run/docker/plugins/debug-socket/
./bin/net-dhcp --sock=/run/docker/plugins/debug-socket/net-dhcp.sock --log debug
```

Expected logs:
```
INFO  Plugin starting up log_level=debug socket=/run/docker/plugins/debug-socket/net-dhcp.sock
INFO  Plugin socket created and accessible socket=/run/docker/plugins/debug-socket/net-dhcp.sock
```

## Test Level 4: Fallback (Development)

For development environments without Docker plugin manager.

### Run in Development Mode

```bash
# Ensure /run/docker/plugins/ is empty or doesn't exist
# Kill any running instances
pkill -f net-dhcp

# Run without any socket configuration
./bin/net-dhcp --log debug
```

Expected logs:
```
INFO  Using fallback socket path fallback=/run/docker/plugins/net-dhcp.sock
INFO  Plugin socket created and accessible socket=/run/docker/plugins/net-dhcp.sock
```

## Integration Test: Create Network and Container

Once the plugin is enabled, test actual network functionality.

### Step 1: Create a Bridge

```bash
sudo ip link add mydchp type bridge
sudo ip addr add 10.0.0.1/24 dev mydchp
sudo ip link set mydchp up
```

### Step 2: Create Network with Plugin

```bash
docker network create \
  --driver docker-network-dhcp \
  --opt bridge=mydchp \
  --opt dhcp-server=<your-dhcp-server-ip> \
  test-dhcp-net
```

### Step 3: Run Container

```bash
docker run -it --rm --network test-dhcp-net ubuntu:latest sh -c 'ip addr show eth0'
```

Expected output:
```
2: eth0: <BROADCAST,RUNNING,MULTICAST> mtu 1500 qdisc pfifo_fast
    inet 10.0.0.x bcast 10.0.0.255 scope global eth0
```

## Troubleshooting

### Plugin Shows as "disabled"

This indicates socket discovery failed. Check logs:

```bash
docker plugin logs docker-network-dhcp | tail -20
```

Common issues:
- Multiple plugin directories in `/run/docker/plugins/` → auto-discovery can't determine which one is yours
- Socket permissions → plugin can't create socket file
- Path doesn't exist → plugin can't write to that location

**Solution**: Try explicit env variable or flag-based socket path.

### Socket Not Found

```bash
# Verify socket actually exists
ls -la /run/docker/plugins/*/net-dhcp.sock

# If not found, check plugin logs for errors
docker plugin logs docker-network-dhcp | grep -i error

# If still nothing, restart plugin
docker plugin disable docker-network-dhcp
docker plugin enable docker-network-dhcp
```

### Multiple Plugin Directories Confuse Auto-Discovery

If running multiple Docker plugins and auto-discovery can't determine the right directory:

```bash
# Check what's in /run/docker/plugins/
ls -la /run/docker/plugins/

# Should see only one or two directories if running single plugin
# If many exist, use explicit socket path via environment variable
```

### Socket Permissions Issue

```bash
# Check socket permissions
ls -la /run/docker/plugins/*/net-dhcp.sock

# Should be 0660 (rw-rw----)
# If permissions are wrong, plugin may not have been able to create it

# Check if directory exists and is writable
ls -ld /run/docker/plugins/abc123def456/
```

## Performance Test

Monitor the plugin under load:

```bash
# Run multiple containers simultaneously
for i in {1..10}; do
  docker run -d --network test-dhcp-net --name test-$i ubuntu:latest sleep 3600 &
done

# Check plugin logs for performance
docker plugin logs docker-network-dhcp | grep -E "(time|latency|duration)" | tail -20

# Clean up
docker ps -q --filter "name=test-" | xargs docker rm -f
```

## Restoration Test

Test that the plugin properly restores endpoints after restart.

### Prerequisites

- Create network and run container (see Integration Test above)
- Container should be running and have IP from DHCP

### Test Steps

```bash
# 1. Check endpoint state before restart
docker ps --filter "network=test-dhcp-net"

# 2. Disable and re-enable plugin
docker plugin disable docker-network-dhcp
docker plugin enable docker-network-dhcp

# 3. Monitor logs for restoration
docker plugin logs docker-network-dhcp | grep -i restore

# Expected logs:
# INFO  Loading persisted endpoint state from disk
# INFO  Starting endpoint restoration
# INFO  Restored endpoint for container <id>

# 4. Verify containers still have IP
docker inspect <container_id> | grep -A 5 Networks

# Should still show DHCP-allocated IP
```

## Success Criteria

✅ Auto-discovery works: Socket created at `/run/docker/plugins/<ID>/net-dhcp.sock`  
✅ Plugin is not marked disabled  
✅ Docker can communicate with plugin  
✅ Containers can be created on the network  
✅ Containers receive DHCP-allocated IPs  
✅ Container IPs survive plugin restart  
✅ All logging shows discovery method used  

## Cleanup

```bash
# Remove test network
docker network rm test-dhcp-net

# Disable and remove plugin
docker plugin disable docker-network-dhcp
docker plugin rm docker-network-dhcp

# Remove bridge
sudo ip link del mydchp

# Clean up containers
docker ps -a -q | xargs docker rm -f
```

## Next Steps

After successful testing:
1. Update plugin version tag
2. Push to Docker Hub/registry
3. Document in release notes
4. Update deployment guides
5. Monitor real-world usage for any issues
