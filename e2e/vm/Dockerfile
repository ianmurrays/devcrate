# The fake Provider's "VM": systemd as PID 1, sshd and dockerd. Run privileged.
# Docker and the devcontainer CLI are baked in where setup looks for them, as
# on a snapshot image; TestSetupInstallsMissingPrerequisites removes them.
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus openssh-server git ca-certificates curl iproute2 xz-utils \
 && install -m 0755 -d /etc/apt/keyrings \
 && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" > /etc/apt/sources.list.d/docker.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends docker-ce docker-ce-cli containerd.io docker-buildx-plugin \
 && rm -rf /var/lib/apt/lists/* \
 && curl -fsSL -o /tmp/devcontainers-install.sh https://raw.githubusercontent.com/devcontainers/cli/main/scripts/install.sh \
 && sh /tmp/devcontainers-install.sh --prefix /opt/devcontainers \
 && rm /tmp/devcontainers-install.sh \
 && systemctl enable ssh docker \
 && systemctl mask getty.target console-getty.service systemd-logind.service systemd-resolved.service \
 && mkdir -p /root/.ssh && chmod 700 /root/.ssh

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
