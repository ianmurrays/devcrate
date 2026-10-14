#!/bin/sh
# devcrate setup: runs on the VM inside `up`'s SSH session as `sh -s -- <origin>`.
# Any failure exits non-zero without the Ready marker, which is written last.
set -eu
origin=$1
workspace=/var/lib/devcrate/workspace

rm -f /run/devcrate/ready

if [ ! -d "$workspace/.git" ]; then
	git clone "$origin" "$workspace"
fi

mkdir -p /run/devcrate
touch /run/devcrate/ready
