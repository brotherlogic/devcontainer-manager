# Devcontainer Manager

This is a project to automatically run and managed devcontainers on a linux instance. It pulls
a set of containers from the container.list.template and then runs and syncs them if they change.

It is written in modern golang, and has access to gh tool for using github.

## Workflow

Once a change is complete, run the workflows/finish.md workflow to add the code to the main branch.