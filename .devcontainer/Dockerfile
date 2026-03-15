FROM mcr.microsoft.com/devcontainers/go:1-1.22-bookworm

# Install devpod
RUN curl -L -o devpod "https://github.com/loft-sh/devpod/releases/latest/download/devpod-linux-amd64" \
    && install -c -m 0755 devpod /usr/local/bin \
    && rm devpod
