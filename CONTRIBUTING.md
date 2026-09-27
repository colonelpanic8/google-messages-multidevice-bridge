# Contributing

Google Messages Multi-Device Bridge is an early prototype built against unofficial and
evolving Google Messages and WhatsApp protocols. Keep changes focused, preserve the explicit
reliability boundaries in the README, and add tests for behavioral changes.

Enter the reproducible development environment with `direnv allow` or
`nix develop`, then run the same checks as CI:

```sh
just check
nix flake check
```

Use `just fmt` to apply Go and Nix formatting. Never commit Google cookies, WhatsApp keys,
pairing data, message databases, API tokens, storage keys, or test fixtures
containing real message data.
