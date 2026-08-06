# Support and compatibility

| Contract | Current development support |
|---|---|
| Go | Exactly 1.26.5 for development and release verification |
| Spice provisional minimum | `v0.0.0-20260805222830-a2ecd56df246` |
| Spice current | `v0.0.0-20260806053623-2ec6f862073f` |
| Operating systems | Windows, Linux, and macOS; Linux integration evidence |
| Architectures | amd64 and arm64 compilation through the core public API |
| Transport security | Verified STARTTLS or implicit TLS; TLS 1.2 minimum |
| Authentication | SMTP AUTH PLAIN only after TLS |
| Real-system acceptance | Mailpit v1.30.0 image digest `sha256:0059ef81e492a7192af3816281eed6859eb078bd7bdc58b76757c13e10e53a7d` |
| Release parity tool | `github.com/spice-framework/development/cmd/spice-dev` at `v0.0.0-20260806132124-4c308d1b9fda` |
| Release verifier tool | `github.com/spice-framework/toolchain/cmd/spice-library-release-verify` at `v0.0.0-20260806133530-71211498297c` |
| Release trust anchor | Configured at `security/release/ed25519-public.pem`; SHA-256 fingerprint `fc7de5d2c7594c6e1871da5f2dc46969d29a9aa2d6a5babaca47fc1ae51b621e` |
| Release secret contract | Only repository secret `SPICE_LIBRARY_RELEASE_SIGNING_KEY` is explicitly mapped to the protected reusable workflow; inheritance is forbidden |

The first preview tag will define the first published minimum Spice version.
Until then, the exact direct Spice requirement in `go.mod` is the provisional
minimum development line. The machine-readable `spice-compatibility.json`
also pins the current compatibility endpoint; that endpoint is a forward-
compatibility signal, not an unbounded runtime dependency.

The compatibility runner resolves both versions through Go's module tooling,
uses isolated alternate modfiles, asserts the exact MVS selection, and then
runs vet plus shuffled race tests over every product package with
`GOPROXY=off`. It rejects any change to product Go files, `go.mod`, `go.sum`, or
vendor. CI publishes parallel minimum/current jobs, while local `make verify`
always proves both boundaries. A release may raise the minimum only through an
intentional `go.mod` change, an updated compatibility manifest and table, and
green minimum/current evidence. A moving branch name is never accepted as a
compatibility version or written to release metadata.

`net/smtp` provides a deliberately bounded protocol subset. SMTPUTF8, DSN,
custom SASL mechanisms, and connection pooling are not claimed. Applications
that require those capabilities should use a separately reviewed transport
behind the public `mail.Sender` interface.

The pinned central signer and independent verifier are the protected production
path. Windows and Linux CI still compare the central renderer with the retained
builder under vendor-only offline resolution; the retained command is a parity
oracle only. The committed public trust anchor establishes the identity against
which future releases must be verified; no signed release is claimed yet.
