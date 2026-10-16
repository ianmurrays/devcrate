# GPG commit signing

devcrate signs commits on an Environment with SSH-format signing only (`gpg.format = ssh`). It does not support GPG (OpenPGP) or X.509 (`gpgsm`) signing.

## Why this is out of scope

SSH signing reuses what devcrate already forwards: a private key file goes to the VM's tmpfs like any other forwarded secret, and `ssh-keygen -Y sign` is on every Environment that has OpenSSH. GitHub verifies SSH signatures the same as GPG ones, so branch protection rules that require signed commits accept them.

GPG signing would need a second set of plumbing: forwarding a gpg-agent socket or a secret keyring to the VM and into the devcontainer, `gpg` installed in every devcontainer image, and a way to unlock the key with nobody connected. That is a lot of surface for something a dedicated SSH signing key already covers. See `docs/adr/0006-commits-are-signed-with-a-tmpfs-key.md`.

Users who must sign with GPG can still set it up themselves with a setup script and `forward_files`.

## Prior requests

- #51: "Sign commits made on an Environment" (GPG support was one of its open questions)
