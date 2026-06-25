#!/bin/bash

# Ensure the 'prod' session exists
if ! tmux has-session -t devcontainer-manager 2>/dev/null; then
  # Create a new session named 'prod', detached
  cd /workspaces/devcontainer-manager
  tmux new-session -d -s devcontainer-manager
fi
