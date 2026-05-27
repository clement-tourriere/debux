# syntax=docker/dockerfile:1

# Pin the multi-platform base image digest for reproducible debug images.
# Dependabot can still propose digest refreshes for this Dockerfile.
FROM nixos/nix:latest@sha256:e2fe74e96e965653c7b8f16ac64d1e56581c63c84d7fa07fb0692fd055cd06b0 AS builder

# Pin nixpkgs so rebuilds do not silently change the toolbox contents.
ARG NIXPKGS_REV=1c3fe55ad329cbcb28471bb30f05c9827f724c76

# Enable flakes (needed for dctl's nix build/search fallbacks)
RUN echo "experimental-features = nix-command flakes" >> /etc/nix/nix.conf && \
    echo "filter-syscalls = false" >> /etc/nix/nix.conf

# Remove git-minimal that ships with the base image (conflicts with full git)
RUN nix-env -e git-minimal 2>/dev/null || true

# Install base packages into a profile. Keep the cleanup in this same layer:
# otherwise Docker retains the removed Nix channels/derivations in lower layers.
RUN set -eux; \
    nix-channel --remove nixpkgs 2>/dev/null || true; \
    nix-channel --remove nixos 2>/dev/null || true; \
    nix-channel --add "https://github.com/NixOS/nixpkgs/archive/${NIXPKGS_REV}.tar.gz" nixpkgs; \
    nix-channel --update; \
    nix-env -iA \
      nixpkgs.zsh \
      nixpkgs.curl \
      nixpkgs.wget \
      nixpkgs.htop \
      nixpkgs.strace \
      nixpkgs.ltrace \
      nixpkgs.vim \
      nixpkgs.jq \
      nixpkgs.less \
      nixpkgs.nettools \
      nixpkgs.iproute2 \
      nixpkgs.procps \
      nixpkgs.findutils \
      nixpkgs.gnugrep \
      nixpkgs.gawk \
      nixpkgs.diffutils \
      nixpkgs.file \
      nixpkgs.tree \
      nixpkgs.tcpdump \
      nixpkgs.dig \
      nixpkgs.nmap \
      nixpkgs.openssh \
      nixpkgs.git \
      nixpkgs.zsh-autosuggestions \
      nixpkgs.zsh-syntax-highlighting; \
    nix-channel --remove nixpkgs 2>/dev/null || true; \
    nix-channel --remove nixos 2>/dev/null || true; \
    nix-env --delete-generations old || true; \
    nix-collect-garbage -d || true; \
    rm -rf /root/.cache /tmp/*

# Reuse the built image instead of starting from nixos/nix again and copying the
# whole store. The previous approach duplicated ~500 MB of base Nix store data.
FROM builder

ARG NIXPKGS_REV=1c3fe55ad329cbcb28471bb30f05c9827f724c76
ENV NIXPKGS_REF="github:NixOS/nixpkgs/${NIXPKGS_REV}"

# nixos/nix ships /etc/passwd and /etc/group as absolute symlinks into
# /nix/store. Kubernetes/containerd opens those files through a constrained
# rootfs mount while creating the container, and absolute symlinks fail with:
#   openat etc/passwd: path escapes from parent
# Materialize both files in /etc so the debug image starts reliably on EKS,
# containerd, and CRI-O based clusters.
RUN set -eux; \
    passwd_src="$(readlink -f /etc/passwd)"; \
    group_src="$(readlink -f /etc/group)"; \
    { echo 'root:x:0:0:root:/root:/bin/sh'; grep -v '^root:' "$passwd_src" || true; } > /tmp/passwd; \
    { echo 'root:x:0:'; grep -v '^root:' "$group_src" || true; } > /tmp/group; \
    rm -f /etc/passwd /etc/group; \
    install -m 0644 /tmp/passwd /etc/passwd; \
    install -m 0644 /tmp/group /etc/group; \
    rm -f /tmp/passwd /tmp/group; \
    test ! -L /etc/passwd; \
    test ! -L /etc/group

RUN mkdir -p /etc/debux

COPY images/debug/dctl /usr/local/bin/dctl
COPY images/debug/zshrc /root/.zshrc
COPY images/debug/zshrc /etc/debux/zshrc
COPY images/debug/command-not-found-handler /etc/zsh/command-not-found-handler
COPY images/debug/entrypoint.sh /entrypoint.sh

# Allow the restricted Kubernetes profile (uid 65534, no extra capabilities) to
# install ad-hoc tools with dctl without making persistent Nix state writable by
# every UID that might run inside the debug container.
RUN set -eux; \
    chmod +x /usr/local/bin/dctl /entrypoint.sh; \
    rm -rf /nix/var/debux-profile; \
    chmod 1777 /tmp; \
    chown 65534:0 /nix/store /nix/var; \
    chmod 0755 /nix/store /nix/var; \
    find /nix/var/nix -xdev -exec chown 65534:0 {} +; \
    find /nix/var/nix -xdev -type d -exec chmod 0755 {} +; \
    find /nix/var/nix -xdev -type f -exec chmod 0644 {} +

ENV PATH="/nix/var/debux-profile/bin:/usr/local/bin:/root/.nix-profile/bin:$PATH"

ENTRYPOINT ["/entrypoint.sh"]
