# SMTP transport dependency review

## Decision

The secure SMTP starter uses Go's standard-library `net/smtp` client behind a
small instance-owned transport. It adds no third-party runtime module.

## Maintenance and compatibility

`net/smtp` is frozen and accepts no new features, but it remains covered by the
Go compatibility promise. The starter uses only the stable operations needed
for authenticated MIME delivery: greeting, EHLO, STARTTLS, AUTH PLAIN,
envelope commands, DATA, and QUIT. SMTPUTF8, DSN, pipelining control, and
custom SASL mechanisms are not claimed.

## License

The Go standard library uses the Go project BSD-style license. There is no
additional distributed dependency. Mailpit is an MIT-licensed hosted test
dependency only and never enters the module or shipped artifact.

## Security

- TLS is mandatory. STARTTLS is required and fail-closed by default; implicit
  TLS is the explicit alternative.
- Certificate verification cannot be disabled, TLS 1.2 is the minimum, and
  renegotiation is rejected.
- Authentication happens only after TLS is active.
- Credentials, recipients, subjects, MIME data, attachments, and raw server
  responses are excluded from observations and public error strings.
- Retry is limited to transient failures before DATA begins. Ambiguous failures
  after message transmission are never replayed automatically.

## Cancellation and ownership

Construction performs no DNS lookup or network operation. Each `Send` attempt
owns one connection, applies the earlier of the caller deadline and configured
timeout, and closes the connection when the context is canceled. No global
client, goroutine, or mutable registry is retained.

## Observability

The optional synchronous observer receives bounded attempt number, message ID,
outcome, SMTP stage/status class, duration, and next backoff only. Applications
may translate those records into their chosen logging, metrics, or tracing
system without exposing message payloads or credentials.

## Build-only dependencies: central Spice release tools

- Decision: approved as the repository-authorized release signer, renderer,
  and independent verifier.
- Version: `github.com/spice-framework/development`
  `v0.0.0-20260806121906-963bb6676069`.
- Tool: `github.com/spice-framework/development/cmd/spice-dev` through the
  standard Go `tool` directive; invocations always use the full package path.
- Verifier: `github.com/spice-framework/toolchain/cmd/spice-library-release-verify`
  from `github.com/spice-framework/toolchain`
  `v0.0.0-20260806054457-a83d9b58034c`, also through the standard Go `tool`
  directive.
- License: Apache-2.0, with its notice retained in `vendor`.
- Runtime scope: none. Product packages do not import the development module,
  and released applications acquire no runtime dependency on it.
- Dependency graph: the tool participates in normal Go minimal-version
  selection. That build-time coupling is accepted and visible in `go.mod`,
  `go.sum`, and `vendor/modules.txt`; no parallel tool registry is introduced.
- Integrity and network behavior: the exact pseudo-version is pinned and
  checksummed. Release parity runs with `GOWORK=off`, `GOPROXY=off`,
  `GOTOOLCHAIN=local`, and `GOFLAGS=-mod=vendor`, so it cannot select an ambient
  checkout, upgrade itself, or download dependencies.
- Security: the trusted native renderer reads the exact committed Git graph
  and writes only to caller-supplied temporary output directories. The
  independent verifier authenticates release artifacts against an external
  trust anchor and exact Git objects. Neither tool generates private material.
- Maintenance: the protected central workflow owns production. The retained
  local builder remains only as the dual-builder parity oracle and is not
  removed by this cutover.
