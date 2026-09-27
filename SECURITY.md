# Security Policy

Google Messages Multi-Device Bridge handles Google and WhatsApp session credentials and private message data.
Do not open a public issue for a suspected vulnerability or include real
credentials, database files, or message contents in a report.

Report vulnerabilities through GitHub's private vulnerability reporting for
this repository. Include a minimal reproduction using synthetic data where
possible.

This project is an early prototype. No released version is currently eligible
for security support, and it should not be exposed directly to the public
internet.


WhatsApp device identity, Signal sessions and app-state keys are encrypted as
records in the same locked bbolt database as messages. There is no plaintext
credential sidecar. The storage key must be backed up separately and never
committed or written into a Nix derivation. Record identifiers and sizes remain
visible. Encryption at rest does not protect against a compromised running process
or host. Ephemeral/view-once content received by the bridge may remain in its local
archive, including after remote expiry or revocation. See
[ADR 0003](docs/adr/0003-whatsapp-linked-device-storage-and-identity.md).
