#!/bin/bash
# Minivisor Remote Update Script
# Based on deploy_remote.sh, focused on binary update and service restart.
# Usage: ./update_remote.sh [ip] [user] [password]

IP=${1:-""}
USER=${2:-""}
PASS=${3:-""}

if [ -z "$IP" ] || [ -z "$USER" ] || [ -z "$PASS" ]; then
    echo "Usage: $0 [ip] [user] [password]"
    exit 1
fi

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
        INIT_TYPE="openrc"
        STOP_CMD="rc-service tinyvisor stop"
        START_CMD="rc-service tinyvisor start"
        STATUS_CMD="rc-service tinyvisor status"
        ;;
    ubuntu|debian)
        INIT_TYPE="systemd"
        STOP_CMD="systemctl stop tinyvisor"
        START_CMD="systemctl start tinyvisor"
        STATUS_CMD="systemctl status tinyvisor"
        ;;
    *)
        echo "Error: Unsupported OS '$OS_ID'. Only Alpine and Ubuntu/Debian are supported."
        exit 1
        ;;
esac

echo "--- Step 3: Stopping Service ($INIT_TYPE) ---"
run_ssh "$STOP_CMD" || echo "Warning: Failed to stop service (it might not be running)"

echo "--- Step 4: Uploading New Binary ---"
run_scp build/tinyvisor "$USER@$IP:/opt/tinyvisor/tinyvisor"
run_ssh "chmod +x /opt/tinyvisor/tinyvisor"

echo "--- Step 5: Starting Service ($INIT_TYPE) ---"
run_ssh "$START_CMD"

echo "--- Step 6: Checking Status ---"
run_ssh "$STATUS_CMD"

echo "--- Update Complete ---"
echo "Visit: http://$IP:7891"
