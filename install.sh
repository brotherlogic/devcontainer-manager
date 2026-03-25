#!/bin/bash

set -e

# Install to user local bin
INSTALL_DIR="$HOME/.local/bin"
BINARY_NAME="devcontainer-manager"
SERVICE_NAME="devcontainer-manager.service"

echo "Building binary..."
go build -o "$BINARY_NAME" main.go

echo "Installing binary to $INSTALL_DIR..."
mkdir -p "$INSTALL_DIR"
cp "$BINARY_NAME" "$INSTALL_DIR/"
chmod +x "$INSTALL_DIR/$BINARY_NAME"

echo "Setting up systemd user service..."

# we must locate the unadulterated service-file in current dir
if [ ! -f "service-file" ]; then
    echo "Could not find 'service-file' in current directory."
    exit 1
fi

USER_SYSTEMD_DIR="$HOME/.config/systemd/user"
mkdir -p "$USER_SYSTEMD_DIR"

PROJECT_DIR=$(pwd)
GO_CMD=$(which go)

# Copy the service file and replace placeholders
sed -e "s|%h/.local/bin/devcontainer-manager|$INSTALL_DIR/$BINARY_NAME|g" \
    -e "s|%PROJECT_DIR%|$PROJECT_DIR|g" \
    -e "s|%GO_CMD%|$GO_CMD|g" \
    service-file > "$USER_SYSTEMD_DIR/$SERVICE_NAME"

# restorecon might be needed on SELinux systems like Silverblue
if command -v restorecon &> /dev/null; then
    restorecon -R "$USER_SYSTEMD_DIR"
fi


echo "Enabling linger..."
loginctl enable-linger "$USER" || echo "Note: Could not enable linger, you may need to do this manually."

echo "Enabling and starting the service..."
systemctl --user daemon-reload
systemctl --user enable --now "$SERVICE_NAME"

echo "Installation complete."
echo "You can check the service status with:"
echo "systemctl --user status $SERVICE_NAME"
