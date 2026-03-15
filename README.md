# Devcontainer Manager

This project pulls in a list of github sourced devcontainers, stored in the container.list file. It then periodically checks github for updates
and if it detects that the devcontainer configuration has changed, it will delete and recreate the new container with the updated configuration.

It assumes the existance of the gh cli for detecting changes to the devcontainer configuration. And additionally assumes that the user has devpod
cli installed for managing devcontainers and running them. Project is written in golang, using the latest standards. 