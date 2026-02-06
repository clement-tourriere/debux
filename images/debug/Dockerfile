FROM nixos/nix:latest AS builder

# Enable flakes (needed for dctl's nix build/search fallbacks)
RUN echo "experimental-features = nix-command flakes" >> /etc/nix/nix.conf

# Remove git-minimal that ships with the base image (conflicts with full git)
RUN nix-env -e git-minimal 2>/dev/null || true

# Install base packages into a profile
RUN nix-channel --update && \
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
      nixpkgs.zsh-syntax-highlighting

FROM nixos/nix:latest

# Enable flakes
RUN echo "experimental-features = nix-command flakes" >> /etc/nix/nix.conf

COPY --from=builder /nix/store /nix/store
COPY --from=builder /nix/var /nix/var
COPY --from=builder /root/.nix-profile /root/.nix-profile

COPY images/debug/dctl /usr/local/bin/dctl
COPY images/debug/zshrc /root/.zshrc
COPY images/debug/command-not-found-handler /etc/zsh/command-not-found-handler
COPY images/debug/entrypoint.sh /entrypoint.sh

RUN chmod +x /usr/local/bin/dctl /entrypoint.sh

ENV PATH="/root/.nix-profile/bin:$PATH"

ENTRYPOINT ["/entrypoint.sh"]
