# The fake Provider's "VM": systemd as PID 1, sshd and dockerd. Run privileged.
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus openssh-server git ca-certificates curl iproute2 \
 && install -m 0755 -d /etc/apt/keyrings \
 && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" > /etc/apt/sources.list.d/docker.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends docker-ce docker-ce-cli containerd.io \
 && rm -rf /var/lib/apt/lists/* \
 && systemctl enable ssh docker \
 && systemctl mask getty.target console-getty.service systemd-logind.service systemd-resolved.service \
 && mkdir -p /root/.ssh && chmod 700 /root/.ssh

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
