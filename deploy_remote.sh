#!/bin/bash
# Minivisor Remote Deployment Script
# Supports Alpine Linux (apk/openrc) and Ubuntu/Debian (apt/systemd)
# Usage: ./deploy_remote.sh [ip] [user] [password]

IP=${1:-"192.168.1.236"}
USER=${2:-"root"}
PASS=${3:-"1"}

SSH_OPTS="-o StrictHostKeyChecking=no"

run_ssh() {
    sshpass -p "$PASS" ssh $SSH_OPTS "$USER@$IP" "$@"
}

run_scp() {
    sshpass -p "$PASS" scp $SSH_OPTS "$@"
}

echo "--- Step 1: Building Minivisor ---"
./build.sh

if [ ! -f "build/tinyvisor" ]; then
    echo "Error: Build failed, binary not found."
    exit 1
fi

echo "--- Step 2: Detecting Remote OS ($IP) ---"
OS_ID=$(run_ssh "cat /etc/os-release 2>/dev/null | grep '^ID=' | cut -d= -f2 | tr -d '\"'")
echo "Detected OS: $OS_ID"

case "$OS_ID" in
    alpine)
        echo "--- Step 3: Preparing Remote Environment (Alpine) ---"
        run_ssh << 'EOF'
            apk update
            apk add bash ca-certificates shadow
            mkdir -p /opt/tinyvisor
EOF
        INIT_TYPE="openrc"
        ;;
    ubuntu|debian)
        echo "--- Step 3: Preparing Remote Environment (Ubuntu/Debian) ---"
        run_ssh << 'EOF'
            apt-get update
            apt-get install -y bash ca-certificates
            mkdir -p /opt/tinyvisor
EOF
        INIT_TYPE="systemd"
        ;;
    *)
        echo "Error: Unsupported OS '$OS_ID'. Only Alpine and Ubuntu/Debian are supported."
        exit 1
        ;;
esac

echo "--- Step 4: Uploading Binary ---"
run_scp build/tinyvisor "$USER@$IP:/opt/tinyvisor/tinyvisor"

echo "--- Step 5: Cleaning Up Old Service Files ---"
if [ "$INIT_TYPE" = "systemd" ]; then
    # Remove any leftover OpenRC init script to avoid SysV fallback
    run_ssh << 'EOF'
        if [ -f /etc/init.d/tinyvisor ]; then
            echo "Removing old OpenRC init script..."
            update-rc.d -f tinyvisor remove 2>/dev/null || true
            rm -f /etc/init.d/tinyvisor
        fi
EOF
else
    # Remove any leftover systemd unit file
    run_ssh << 'EOF'
        if [ -f /etc/systemd/system/tinyvisor.service ]; then
            echo "Removing old systemd unit file..."
            systemctl disable tinyvisor 2>/dev/null || true
            rm -f /etc/systemd/system/tinyvisor.service
            systemctl daemon-reload 2>/dev/null || true
        fi
EOF
fi

echo "--- Step 6: Installing $INIT_TYPE Service ---"
run_ssh << EOF
    chmod +x /opt/tinyvisor/tinyvisor
    # Run from the directory to ensure config.json is created there
    cd /opt/tinyvisor
    ./tinyvisor -service-install $INIT_TYPE

    # Ensure tinyvisor user owns the directory (best-effort, may fail if user creation failed)
    chown -R tinyvisor:tinyvisor /opt/tinyvisor 2>/dev/null || chown -R tinyvisor /opt/tinyvisor 2>/dev/null || true
EOF

if [ "$INIT_TYPE" = "openrc" ]; then
    run_ssh << 'EOF'
        rc-update add tinyvisor default
        rc-service tinyvisor restart
EOF
else
    run_ssh << 'EOF'
        systemctl daemon-reload
        systemctl enable tinyvisor
        systemctl restart tinyvisor
EOF
fi

echo "--- Deployment Complete ---"
echo "Visit: http://$IP:7891"

# Read pairing PIN from remote config
PIN=$(run_ssh "grep pairingPIN /opt/tinyvisor/config.json" | sed 's/.*: *"\([^"]*\)".*/\1/')
if [ -n "$PIN" ]; then
    echo "Pairing PIN: $PIN"
fi
