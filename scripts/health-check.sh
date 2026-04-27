#!/bin/bash
# Health check script for Docker Network DHCP plugin
# Verifies socket is responsive and plugin is functioning

set -e

SOCKET="/run/docker/plugins/net-dhcp.sock"
TIMEOUT=5

# Check if socket exists
if [ ! -S "$SOCKET" ]; then
    echo "FAIL: Socket not found at $SOCKET"
    exit 1
fi

# Check socket permissions
PERMS=$(stat -c "%a" "$SOCKET" 2>/dev/null || stat -f "%A" "$SOCKET" 2>/dev/null || echo "unknown")
echo "Socket permissions: $PERMS"

# Verify socket is responsive by calling GetCapabilities
RESPONSE=$(timeout $TIMEOUT curl -s --unix-socket "$SOCKET" \
    -X POST http://localhost/NetworkDriver.GetCapabilities \
    -H 'Content-Type: application/json' \
    -d '{}' 2>/dev/null || echo "")

if [ -z "$RESPONSE" ]; then
    echo "FAIL: Socket not responding to GetCapabilities"
    exit 1
fi

# Verify response contains expected fields
if echo "$RESPONSE" | grep -q "Scope"; then
    echo "OK: Plugin socket is responsive"
    echo "$RESPONSE" | jq . 2>/dev/null || echo "$RESPONSE"
    exit 0
else
    echo "FAIL: Invalid response from plugin"
    echo "$RESPONSE"
    exit 1
fi
