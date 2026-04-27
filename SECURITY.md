# Security Model

## Unix Socket Permissions

The plugin communicates with Docker daemon via a Unix domain socket at `/run/docker/plugins/net-dhcp.sock`.

### Socket Access Control

**Current Implementation:**
- Socket file is created by `net.Listen("unix", bindSock)` with default Go permissions (mode 0644)
- This allows any process with filesystem access to connect to the socket
- Docker daemon typically runs as root, mitigating the risk in container environments

**Permission Structure:**
```
Socket:     /run/docker/plugins/net-dhcp.sock
Owner:      root:root (or docker user)
Mode:       0644 (read/write for owner, read-only for others)
Listener:   Single-threaded HTTP server via gorilla/handlers
```

### Security Implications by Deployment Model

#### Container Deployment (Production)
✅ **LOW RISK**
- Plugin runs in container with root privileges
- Socket lives in mounted volume `/run/docker/plugins`
- Host filesystem access is restricted by container runtime
- Only dockerd can reliably connect to the socket

#### Host Deployment (Development/Testing)
⚠️ **MEDIUM RISK**
- Any process on the host with filesystem access can connect
- Network driver API calls could be intercepted or spoofed
- Mitigation: Run plugin as dedicated docker user with restricted umask

**Recommended Configuration for Host Deployment:**
```bash
# Create dedicated user
useradd -r -s /usr/sbin/nologin docker-plugin

# Run with restricted permissions
sudo -u docker-plugin /usr/sbin/net-dhcp --sock=/run/docker/plugins/net-dhcp.sock

# Verify socket permissions
ls -la /run/docker/plugins/net-dhcp.sock
# Expected: docker-plugin:docker-plugin 0600
```

## Error Handling & Information Disclosure

### Logging vs. Client Responses

The plugin follows secure error handling practices:

- **Internal Logs** (Server-side): Full error details including stack traces, source paths, query information
- **Client Responses**: Generic error messages without sensitive information

**Implementation:**
```go
// Internal logging captures full context
log.WithFields(log.Fields{
    "error":       clientMsg,
    "status_code": statusCode,
}).Error("API error response")

// Client receives only status code and generic message
{"Err": "error message"}
```

### Sensitive Data Protection

The following information is **never sent to clients**:
- Filesystem paths
- Container IDs (full, only shortened in logs)
- Network interface names (internal use only)
- Bridge configuration details
- Namespace paths

## API Version Negotiation

The plugin uses automatic API version negotiation with Docker daemon via `WithAPIVersionNegotiation()`.

**Security Impact:**
- Ensures compatibility with multiple Docker versions
- No hardcoded API version bypass
- Full version negotiation on first request

## Panic Recovery

All goroutines implement panic recovery to prevent information disclosure:

- **HTTP Server Goroutine**: Recovers panics with structured logging
- **Restore Goroutine**: Recovers panics during state restoration
- **DHCP Manager Goroutines**: Recovers panics per endpoint

Panic stack traces are logged internally but never exposed to Docker daemon.

## Network Isolation

### DHCP Requests

DHCP protocol traffic:
- Uses UDP port 67/68 on the configured bridge interface
- Requests originate from container namespace
- No exposure via HTTP API socket

### Docker Network API

Network driver API endpoints:
- Only accessible via local Unix socket
- JSON-over-HTTP protocol
- No network exposure (host-local only)

## Dependency Security

### Go Language

- Uses Go 1.26.2+ with security patches included
- Covers: crypto/x509, html/template, net/url, compiler memory safety
- Rebuild recommended when Go releases critical patches

### Third-Party Dependencies

All dependencies are from well-maintained projects:
- `github.com/moby/moby/client` — Docker's official client SDK
- `github.com/sirupsen/logrus` — Industry-standard logging
- `github.com/vishvananda/netlink` — Kernel netlink interface
- `github.com/gorilla/handlers` — HTTP middleware (logging)

**Dependency Updates:**
```bash
# Check for vulnerabilities
go list -json -m all | nancy sleuth

# Update dependencies
go get -u ./...
go mod tidy
```

## Recommended Security Practices

### Deployment
1. Run plugin in container with minimal privileges when possible
2. Use read-only filesystem where practical
3. Enable audit logging at Docker daemon level
4. Monitor socket file permissions regularly

### Operation
1. Set appropriate log level (INFO for production, DEBUG for troubleshooting)
2. Monitor for panic recovery messages in logs
3. Rotate logs regularly to prevent disk space issues
4. Keep Go toolchain updated with security patches

### Testing
1. Verify socket permissions after startup
2. Test error responses do not leak sensitive information
3. Audit logs for unintended exposure
4. Run security scanning tools on container image

## Reporting Security Issues

If you discover a security vulnerability:
1. **Do not** open a public GitHub issue
2. Contact maintainers privately with details
3. Allow time for assessment and patching
4. Follow responsible disclosure practices

## References

- [Go Security Best Practices](https://go.dev/doc/security/best-practices)
- [Docker Security](https://docs.docker.com/engine/security/)
- [Unix Socket Security](https://en.wikipedia.org/wiki/Unix_domain_socket#Security)
- [OWASP: Sensitive Data Exposure](https://owasp.org/www-project-top-ten/)
