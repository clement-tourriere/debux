# Current glibc-based Wolfi packages, not a source-built package-manager closure.
# Pin the multi-architecture base and mise artifacts; refresh packages on rebuild.
FROM cgr.dev/chainguard/wolfi-base:latest@sha256:918a593b8268c222afd4e2c4f06860ac984e60719b4697e4c71d796bc8fcd042
ARG TARGETARCH
ARG MISE_VERSION=2026.9.3

RUN apk upgrade --no-cache && apk add --no-cache \
      bash coreutils util-linux iputils ncurses ca-certificates \
      sed libarchive-tools gzip xz bzip2 unzip zsh curl wget htop strace ltrace \
      gdb tmux ggshield vim jq less net-tools iproute2 procps findutils grep gawk \
      diffutils file tree tcpdump bind-tools nmap socat openssh-client openssl git \
      build-base cmake ninja autoconf automake libtool bison flex linux-headers \
      openssl-dev zlib-dev readline-dev libffi-dev icu-dev util-linux-dev \
      bzip2-dev xz-dev sqlite-dev ncurses-dev \
    && mkdir -p /usr/share/debux \
    && apk info -v | sort > /usr/share/debux/base-packages.txt \
    # gdb-multiarch bundles ~150 MB of static development archives; no runtime
    # tool uses them. Remove in this layer so they don't inflate image downloads.
    && rm -f /usr/lib/libbfd.a /usr/lib/libopcodes.a /usr/lib/libctf.a /usr/lib/libctf-nobfd.a \
    # Keep static linking/LTO support, but not ~150 MB of dependency DWARF data.
    && strip --strip-debug /usr/lib/libstdc++.a /usr/lib/libc.a /usr/lib/libcrypto.a \
      /usr/lib/libasan.a /usr/lib/libtsan.a /usr/lib/libubsan.a /usr/lib/liblsan.a \
    # Debug tools do not need setuid/setgid authentication helpers.
    && find /usr -xdev -type f \( -perm -4000 -o -perm -2000 \) -exec chmod a-s {} +

# Source-only mise backends use the shared build toolchain above, without
# installing system packages/elevating privileges at runtime. Installed tools
# and their metadata stay in the writable volume, not in this image.
# Verify official prebuilt mise binaries; no Nix closure/source overlay is needed.
RUN set -eu; \
    case "$TARGETARCH" in \
      arm64) arch=arm64; hash=d8fa3d3fa2a21979c54c548967325a9db72f2a157996e8c536f62947b25592b7;; \
      amd64) arch=x64; hash=981bd9179cc089114a87b491fce2c4007ab05b5445dee7ad08e87e5dffcc154d;; \
      *) echo "Unsupported architecture: $TARGETARCH" >&2; exit 1;; \
    esac; \
    curl -fsSL --retry 3 "https://github.com/jdx/mise/releases/download/v${MISE_VERSION}/mise-v${MISE_VERSION}-linux-${arch}" -o /usr/local/bin/mise; \
    printf '%s  /usr/local/bin/mise\n' "$hash" | sha256sum -c -; \
    chmod 0755 /usr/local/bin/mise

# Wolfi doesn't package these zsh plugins. Copy only runtime scripts from
# immutable, checksum-verified upstream sources (no plugin manager at startup).
RUN set -eu; \
    mkdir -p /usr/share/zsh-autosuggestions /usr/share/zsh-syntax-highlighting; \
    curl -fsSL --retry 3 https://raw.githubusercontent.com/zsh-users/zsh-autosuggestions/e52ee8ca55bcc56a17c828767a3f98f22a68d4eb/zsh-autosuggestions.zsh \
      -o /usr/share/zsh-autosuggestions/zsh-autosuggestions.zsh; \
    echo 'eec7ba8f7a71414ace0ea0fab0908d005b24cf65d83b169c0ff97815d3cfc51a  /usr/share/zsh-autosuggestions/zsh-autosuggestions.zsh' | sha256sum -c -; \
    curl -fsSL --retry 3 https://github.com/zsh-users/zsh-syntax-highlighting/archive/db085e4661f6aafd24e5acb5b2e17e4dd5dddf3e.tar.gz -o /tmp/highlighting.tgz; \
    echo '874999413d147b7ad6776077e1b756d31d68284f9ef2ef240b33a4c842f0e1d8  /tmp/highlighting.tgz' | sha256sum -c -; \
    mkdir /tmp/highlighting; \
    bsdtar -xf /tmp/highlighting.tgz -C /tmp/highlighting --strip-components 1; \
    cp /tmp/highlighting/zsh-syntax-highlighting.zsh /tmp/highlighting/.version /tmp/highlighting/.revision-hash /usr/share/zsh-syntax-highlighting/; \
    cp -r /tmp/highlighting/highlighters /usr/share/zsh-syntax-highlighting/; \
    rm -rf /tmp/highlighting /tmp/highlighting.tgz

LABEL org.opencontainers.image.source="https://github.com/clement-tourriere/debux" \
      io.debux.toolbox="mise-v1"
ENV PATH="/var/lib/debux/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
    HOME=/root TERM=xterm-256color LANG=C.UTF-8

COPY images/debug/environment.sh /etc/debux/environment.sh
COPY images/debug/dctl /usr/local/bin/dctl
COPY images/debug/tool-launcher /usr/local/libexec/debux-tool
COPY internal/entrypoint/zshrc /root/.zshrc
COPY internal/entrypoint/zshrc /etc/debux/zshrc
COPY internal/entrypoint/script.sh /tmp/debux-entrypoint-template.sh
RUN set -eu; \
    awk '{if ($0 == "__DEBUX_ZSHRC__") {while ((getline line < "/etc/debux/zshrc") > 0) print line} else print}' \
      /tmp/debux-entrypoint-template.sh > /entrypoint.sh; \
    rm /tmp/debux-entrypoint-template.sh; \
    chmod 0755 /entrypoint.sh /usr/local/bin/dctl /usr/local/libexec/debux-tool; \
    ln -sf /bin/bash /bin/sh; \
    mkdir -p /etc/profile.d /etc/debux/mise; \
    printf '%s\n' '. /etc/debux/environment.sh' > /etc/profile.d/debux.sh; \
    install -d -o 65534 -g 0 -m 0755 /var/lib/debux /var/lib/debux/mise /var/lib/debux/.bin.empty; \
    install -o 65534 -g 0 -m 0644 /dev/null /var/lib/debux/mise/config.toml; \
    ln -s /var/lib/debux/.bin.empty /var/lib/debux/bin; \
    chmod 1777 /tmp; \
    ln -sf ncat /usr/bin/nc; \
    awk -F: '$3 == 65534 { found=1 } END { exit !found }' /etc/passwd; \
    awk -F: '$3 == 65534 { found=1 } END { exit !found }' /etc/group; \
    test ! -L /etc/passwd; test ! -L /etc/group

ENTRYPOINT ["/entrypoint.sh"]
