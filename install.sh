#!/bin/bash

set -e

# Identify the target user and their home directory
TARGET_USER="${SUDO_USER:-$USER}"
TARGET_HOME=$(getent passwd "$TARGET_USER" | cut -d: -f6)
TARGET_UID=$(id -u "$TARGET_USER")

INSTALL_DIR="$TARGET_HOME/.local/bin"
BINARY_NAME="devcontainer-manager"
SERVICE_NAME="devcontainer-manager.service"

echo "Building binary..."
# Build as the target user to maintain Go environment/cache
if [ "$EUID" -eq 0 ]; then
    sudo -u "$TARGET_USER" go build -o "$BINARY_NAME" main.go
else
    go build -o "$BINARY_NAME" main.go
fi

echo "Installing binary to $INSTALL_DIR..."
# Ensure directory exists as target user
if [ "$EUID" -eq 0 ]; then
    sudo -u "$TARGET_USER" mkdir -p "$INSTALL_DIR"
    mv "$BINARY_NAME" "$INSTALL_DIR/"
    chown "$TARGET_USER:$TARGET_USER" "$INSTALL_DIR/$BINARY_NAME"
else
    mkdir -p "$INSTALL_DIR"
    mv "$BINARY_NAME" "$INSTALL_DIR/"
fi
chmod +x "$INSTALL_DIR/$BINARY_NAME"

echo "Setting up systemd user service..."

# we must locate the unadulterated service-file in current dir
if [ ! -f "service-file" ]; then
    echo "Could not find 'service-file' in current directory."
    exit 1
fi

USER_SYSTEMD_DIR="$TARGET_HOME/.config/systemd/user"
if [ "$EUID" -eq 0 ]; then
    sudo -u "$TARGET_USER" mkdir -p "$USER_SYSTEMD_DIR"
else
    mkdir -p "$USER_SYSTEMD_DIR"
fi

PROJECT_DIR=$(pwd)
GO_CMD=$(which go)

# Copy the service file and replace placeholders
sed -e "s|%h/.local/bin/devcontainer-manager|$INSTALL_DIR/$BINARY_NAME|g" \
    -e "s|%PROJECT_DIR%|$PROJECT_DIR|g" \
    -e "s|%GO_CMD%|$GO_CMD|g" \
    service-file > "$BINARY_NAME.tmp"

if [ "$EUID" -eq 0 ]; then
    mv "$BINARY_NAME.tmp" "$USER_SYSTEMD_DIR/$SERVICE_NAME"
    chown "$TARGET_USER:$TARGET_USER" "$USER_SYSTEMD_DIR/$SERVICE_NAME"
else
    mv "$BINARY_NAME.tmp" "$USER_SYSTEMD_DIR/$SERVICE_NAME"
fi

# restorecon might be needed on SELinux systems like Silverblue
if command -v restorecon &> /dev/null; then
    restorecon -R "$USER_SYSTEMD_DIR"
fi

echo "Enabling linger..."
# loginctl might require root privileges
loginctl enable-linger "$TARGET_USER" || echo "Note: Could not enable linger, check manual setup if needed."

echo "Enabling and starting the service..."
if [ "$EUID" -eq 0 ]; then
    sudo -u "$TARGET_USER" XDG_RUNTIME_DIR="/run/user/$TARGET_UID" systemctl --user daemon-reload
    sudo -u "$TARGET_USER" XDG_RUNTIME_DIR="/run/user/$TARGET_UID" systemctl --user enable --now "$SERVICE_NAME"
else
    systemctl --user daemon-reload
    systemctl --user enable --now "$SERVICE_NAME"
fi

echo "Installation complete."
echo "You can check the service status with:"
echo "systemctl --user status $SERVICE_NAME"
