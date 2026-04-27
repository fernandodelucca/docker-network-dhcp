# Socket Path Discovery Mechanism

## Problem

When Docker creates a plugin, it assigns a unique `PLUGIN_ID` and creates the plugin's socket directory at:
```
/run/docker/plugins/<PLUGIN_ID>/net-dhcp.sock
```

However, the plugin doesn't inherently know its `PLUGIN_ID`. The plugin needs to discover this path automatically to bind the socket at the correct location that Docker expects.

**Without proper discovery**, the plugin would try to bind at a hardcoded path like `/run/docker/plugins/net-dhcp.sock`, which doesn't match what Docker created. This causes Docker to not find the socket, mark the plugin as disabled, and fail to restore endpoints.

## Solution

The `discoverSocketPath()` function in `cmd/net-dhcp/main.go` implements a 4-level discovery strategy:

### Level 1: Explicit Flag
```bash
./net-dhcp --sock=/custom/socket/path
```
If a socket path is explicitly provided via command-line flag, use it directly.

### Level 2: Environment Variable
```bash
export DOCKER_PLUGIN_SOCKET=/run/docker/plugins/<PLUGIN_ID>/net-dhcp.sock
docker plugin install net-dhcp:latest
```
If the `DOCKER_PLUGIN_SOCKET` environment variable is set, use that value. This allows users to override socket discovery if needed.

The environment variable can be configured in `config.json`:
```json
{
  "env": [
    {
      "name": "DOCKER_PLUGIN_SOCKET",
      "description": "Override automatic socket path discovery (optional)",
      "value": ""
    }
  ]
}
```

### Level 3: Auto-Discovery
When the plugin runs as a Docker plugin container, Docker mounts `/run/docker/plugins/<PLUGIN_ID>/` inside the container at the same path. The auto-discovery logic:

1. Checks if `/run/docker/plugins/` exists and is a directory
2. Lists all subdirectories (plugin IDs)
3. If **exactly one** subdirectory is found, assumes it's the plugin's directory
4. Constructs the socket path: `/run/docker/plugins/<PLUGIN_ID>/net-dhcp.sock`

**Example:**
```
# Host filesystem
/run/docker/plugins/abc123def456/  ← mounted in container as-is

# Plugin container
# discovers /run/docker/plugins/abc123def456/ exists alone
# constructs: /run/docker/plugins/abc123def456/net-dhcp.sock
```

### Level 4: Fallback
If all discovery methods fail (e.g., running in development without Docker plugin manager), use the hardcoded fallback path:
```
/run/docker/plugins/net-dhcp.sock
```

This allows the plugin to run in development/testing scenarios without Docker's plugin management.

## Logging

Each discovery step logs its action:

```
INFO  Plugin starting up log_level=info socket=/run/docker/plugins/<ID>/net-dhcp.sock

# Level 2 (env var)
INFO  Socket path from environment variable source=DOCKER_PLUGIN_SOCKET

# Level 3 (auto-discovery success)
INFO  Auto-discovered socket path auto_discovered=/run/docker/plugins/<ID>/net-dhcp.sock

# Level 3 (multiple plugins found - cannot auto-discover)
WARN  Multiple plugin directories found, cannot auto-discover plugin_dirs=[id1,id2] count=2

# Level 4 (fallback)
INFO  Using fallback socket path fallback=/run/docker/plugins/net-dhcp.sock
```

## Deployment Scenarios

### Scenario 1: Docker Plugin Manager (Production)
```bash
docker plugin install net-dhcp:latest
```
- Docker creates `/run/docker/plugins/<PLUGIN_ID>/`
- Plugin auto-discovers the directory (Level 3)
- Socket is created at the correct location
- Docker can find and communicate with the plugin

### Scenario 2: Manual Socket Path (Custom Deployment)
```bash
export DOCKER_PLUGIN_SOCKET=/my/custom/socket/path
./net-dhcp
```
- Plugin uses the environment variable (Level 2)
- Socket is created at the specified path

### Scenario 3: Development/Testing
```bash
./net-dhcp
```
- No Docker plugin manager, no `/run/docker/plugins/<PLUGIN_ID>/`
- All discovery methods fail
- Plugin uses fallback path (Level 4)
- Useful for testing without Docker's plugin infrastructure

### Scenario 4: Explicit Flag
```bash
./net-dhcp --sock=/run/docker/plugins/xyz789/net-dhcp.sock
```
- Plugin uses explicit flag value (Level 1)
- Highest priority, useful for debugging or specific deployments

## Integration with Docker's Plugin Lifecycle

### Plugin Installation
```bash
docker plugin install net-dhcp:latest
```

1. Docker creates `/run/docker/plugins/<PLUGIN_ID>/`
2. Docker creates a plugin container with mounted volumes
3. Docker runs the entrypoint from `config.json`: `["/usr/sbin/net-dhcp"]`
4. Plugin starts with no arguments, auto-discovers the socket directory
5. Plugin binds socket at `/run/docker/plugins/<PLUGIN_ID>/net-dhcp.sock`
6. Docker detects the socket and marks the plugin as ready

### Plugin Enable/Restore
```bash
docker plugin enable net-dhcp
```

1. Docker tries to call `GetCapabilities` at the socket location
2. Plugin responds, proving it's ready
3. Docker calls `RestoreEndpoints` to reconnect containers to the network
4. Plugin re-establishes DHCP clients for those containers

## Socket Verification

After the plugin starts listening, it logs socket information:

```
INFO  Plugin socket created and accessible socket=/run/docker/plugins/<ID>/net-dhcp.sock mode=0660 size=0
```

This verifies:
- Socket file exists at the discovered path
- Socket is readable/writable (permissions: 0660)
- Socket is empty (no prior connection data)

## Troubleshooting

### Plugin marked as "disabled"
If Docker marks the plugin as disabled, the socket path is likely wrong:
```bash
# Check where Docker expects the socket
docker plugin inspect net-dhcp | grep -i socket

# Check if socket exists at expected location
ls -la /run/docker/plugins/*/

# Enable verbose logging
docker plugin install net-dhcp:latest --env LOG_LEVEL=debug

# Check plugin logs
docker plugin logs net-dhcp | grep socket
```

### Multiple plugins cause auto-discovery to fail
If running multiple plugins in the same `/run/docker/plugins/` directory:
```bash
# Solution 1: Set explicit environment variable
docker plugin install net-dhcp:latest --env DOCKER_PLUGIN_SOCKET=/run/docker/plugins/<YOUR_ID>/net-dhcp.sock

# Solution 2: Run plugins in separate Docker instances
```

### Socket file permission denied
If the socket exists but has wrong permissions:
```bash
# Plugin logs should indicate this
docker plugin logs net-dhcp | grep -i permission

# Socket should be created with mode 0660 (rw for user and group)
ls -la /run/docker/plugins/*/net-dhcp.sock
```

## Technical Details

### Why Auto-Discovery Works
- Docker mounts `/run/docker/plugins/<PLUGIN_ID>/` inside the plugin container at the exact same path
- When listing `/run/docker/plugins/`, the plugin sees only its own mounted directory
- This makes auto-discovery reliable without needing to pass extra configuration

### Why Not Use Environment Variables by Default?
- Docker's plugin manager doesn't automatically set socket-related environment variables
- Environment variables must be explicitly configured in `config.json`
- Auto-discovery is more robust and requires no configuration

### Why Have a Fallback?
- Allows plugin development and testing without Docker's plugin manager
- Useful for debugging in development environments
- Provides graceful degradation if discovery fails

## References

- [Docker Plugin Protocol](https://docs.docker.com/engine/extend/plugin_api/)
- [libnetwork Remote Driver](https://github.com/moby/libnetwork/blob/master/docs/remote.md)
- [Docker Plugin File Structure](https://docs.docker.com/engine/extend/#plugin-json)
