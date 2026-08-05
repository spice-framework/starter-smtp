# Support and compatibility

| Contract | Current development support |
|---|---|
| Go | Exactly 1.26.5 for development and release verification |
| Spice | `v0.0.0-20260805162230-a0bbb964bf6b` |
| Operating systems | Windows, Linux, and macOS; Linux integration evidence |
| Architectures | amd64 and arm64 compilation through the core public API |
| Transport security | Verified STARTTLS or implicit TLS; TLS 1.2 minimum |
| Authentication | SMTP AUTH PLAIN only after TLS |
| Real-system acceptance | Mailpit v1.30.0 image digest `sha256:0059ef81e492a7192af3816281eed6859eb078bd7bdc58b76757c13e10e53a7d` |

The first preview tag will define the minimum supported Spice version. Until
then, development commits intentionally declare one exact compatible Spice
commit and fail closed outside that tested combination. Future releases will
test both the published minimum and the current supported Spice line before
raising that floor.

`net/smtp` provides a deliberately bounded protocol subset. SMTPUTF8, DSN,
custom SASL mechanisms, and connection pooling are not claimed. Applications
that require those capabilities should use a separately reviewed transport
behind the public `mail.Sender` interface.
