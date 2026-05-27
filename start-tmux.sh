#!/bin/bash

# Ensure the 'prod' session exists
if ! tmux has-session -t prod 2>/dev/null; then
  # Create a new session named 'prod', detached
  cd /workspaces/seraphine
  tmux new-session -d -s dcm
  
  # Split the window horizontally (-h)
  # The left pane will remain a terminal
  # The right pane will run 'gh dash'
  tmux split-window -h -t dcm "gh dash"
fi
