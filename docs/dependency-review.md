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
