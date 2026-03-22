#!/bin/bash

set -e

# Support Fedora Silverblue and basically any other system by installing to /usr/local/bin
INSTALL_DIR="/usr/local/bin"
BINARY_NAME="devcontainer-manager"
SERVICE_NAME="devcontainer-manager.service"

if [ "$EUID" -ne 0 ]; then
  echo "Please run as root (e.g. sudo ./install.sh)"
  exit 1
fi

if [ -z "$SUDO_USER" ]; then
    echo "Could not determine SUDO_USER. Please run via sudo."
    exit 1
fi

# We build as the regular user to use their go cache, env, etc.
echo "Building binary..."
sudo -u "$SUDO_USER" go build -o "$BINARY_NAME" main.go

echo "Installing binary to $INSTALL_DIR..."
mkdir -p "$INSTALL_DIR"
mv "$BINARY_NAME" "$INSTALL_DIR/"
chmod +x "$INSTALL_DIR/$BINARY_NAME"

echo "Setting up systemd user service for $SUDO_USER..."

# we must locate the unadulterated service-file in current dir
if [ ! -f "service-file" ]; then
    echo "Could not find 'service-file' in current directory."
    exit 1
fi

USER_UID=$(id -u "$SUDO_USER")
USER_SYSTEMD_DIR="/home/$SUDO_USER/.config/systemd/user"

# create the directory if it doesn't already exist
sudo -u "$SUDO_USER" mkdir -p "$USER_SYSTEMD_DIR"

# Copy the service file but replace the ExecStart path with the global path
sed "s|%h/.local/bin/devcontainer-manager|$INSTALL_DIR/$BINARY_NAME|g" service-file > /tmp/$SERVICE_NAME
chown "$SUDO_USER:$SUDO_USER" /tmp/$SERVICE_NAME

mv /tmp/$SERVICE_NAME "$USER_SYSTEMD_DIR/$SERVICE_NAME"
# restorecon might be needed on SELinux systems like Silverblue
if command -v restorecon &> /dev/null; then
    restorecon -R "$USER_SYSTEMD_DIR"
fi


echo "Enabling linger for $SUDO_USER..."
loginctl enable-linger "$SUDO_USER"

echo "Enabling and starting the service..."
# Run systemctl as the user to enable and start
sudo -u "$SUDO_USER" XDG_RUNTIME_DIR="/run/user/$USER_UID" systemctl --user daemon-reload
sudo -u "$SUDO_USER" XDG_RUNTIME_DIR="/run/user/$USER_UID" systemctl --user enable --now "$SERVICE_NAME"

echo "Installation complete."
echo "You can check the service status with:"
echo "systemctl --user status $SERVICE_NAME"
