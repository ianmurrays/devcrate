#!/bin/sh
# devcrate user data: the Provider runs this at boot, after mounting the
# persistent volume at /var/lib/devcrate. Nothing secret goes here.
systemctl start ssh 2>/dev/null || systemctl start sshd
