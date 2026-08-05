//go:build integration

package integration_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spice-framework/spice/mail"
	spicesmtp "github.com/spice-framework/starter-smtp"
)

const integrationTimeout = time.Minute

func TestMailpitAuthenticatedStartTLSDelivery(t *testing.T) {
	address := requireEnvironment(t, "SPICE_SMTP_ADDRESS")
	api := strings.TrimRight(requireEnvironment(t, "SPICE_SMTP_API"), "/")
	username := requireEnvironment(t, "SPICE_SMTP_USERNAME")
	password := requireEnvironment(t, "SPICE_SMTP_PASSWORD")

	ctx, cancel := context.WithTimeout(t.Context(), integrationTimeout)
	defer cancel()
	certificate, err := startTLSCertificate(ctx, address)
	if err != nil {
		t.Fatalf("discover Mailpit STARTTLS certificate: %v", err)
	}
	if err := awaitCertificateValidity(ctx, certificate); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	sender, err := spicesmtp.New(spicesmtp.Config{
		Address:    address,
		ServerName: "localhost",
		Mode:       spicesmtp.TLSModeStartTLS,
		Username:   username,
		Password:   password,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		},
		Timeout:     5 * time.Second,
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatalf("smtp.New() error = %v", err)
	}
	const body = "Spice standalone SMTP Mailpit acceptance"
	message, err := mail.NewMessage(mail.MessageSpec{
		ID:       "starter-smtp-mailpit@example.test",
		Date:     time.Date(2026, time.August, 5, 16, 0, 0, 0, time.UTC),
		From:     "Spice <spice@example.test>",
		To:       []string{"developer@example.test"},
		Subject:  "Spice SMTP integration",
		TextBody: body,
		Attachments: []mail.AttachmentSpec{{
			Filename:    "evidence.txt",
			ContentType: "text/plain; charset=utf-8",
			Data:        []byte("inspectable attachment"),
		}},
	})
	if err != nil {
		t.Fatalf("mail.NewMessage() error = %v", err)
	}
	if err := sender.Send(ctx, message); err != nil {
		t.Fatalf("Sender.Send() error chain = %s", errorChain(err))
	}
	if err := awaitCapturedBody(ctx, api+"/view/latest.txt", body); err != nil {
		t.Fatal(err)
	}
}

func startTLSCertificate(ctx context.Context, address string) (*x509.Certificate, error) {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dial SMTP endpoint: %w", err)
	}
	defer func() { _ = connection.Close() }()
	protocol := textproto.NewConn(connection)
	if _, _, err := protocol.ReadResponse(220); err != nil {
		return nil, fmt.Errorf("read SMTP greeting: %w", err)
	}
	id, err := protocol.Cmd("EHLO starter-smtp-integration")
	if err != nil {
		return nil, fmt.Errorf("write SMTP EHLO: %w", err)
	}
	protocol.StartResponse(id)
	_, capabilities, err := protocol.ReadResponse(250)
	protocol.EndResponse(id)
	if err != nil {
		return nil, fmt.Errorf("read SMTP EHLO: %w", err)
	}
	if !strings.Contains(strings.ToUpper(capabilities), "STARTTLS") {
		return nil, errorsNewMissingStartTLS(capabilities)
	}
	id, err = protocol.Cmd("STARTTLS")
	if err != nil {
		return nil, fmt.Errorf("write SMTP STARTTLS: %w", err)
	}
	protocol.StartResponse(id)
	_, _, err = protocol.ReadResponse(220)
	protocol.EndResponse(id)
	if err != nil {
		return nil, fmt.Errorf("read SMTP STARTTLS: %w", err)
	}
	// #nosec G402 -- this one bootstrap handshake reads the ephemeral local
	// Mailpit fixture certificate; the product sender then verifies that exact
	// certificate with InsecureSkipVerify disabled.
	secure := tls.Client(connection, &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err := secure.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("negotiate fixture TLS: %w", err)
	}
	certificates := secure.ConnectionState().PeerCertificates
	if len(certificates) == 0 {
		return nil, fmt.Errorf("fixture TLS returned no peer certificate")
	}
	return certificates[0], nil
}

func errorsNewMissingStartTLS(capabilities string) error {
	return fmt.Errorf("SMTP fixture does not advertise STARTTLS: %q", capabilities)
}

func awaitCertificateValidity(ctx context.Context, certificate *x509.Certificate) error {
	delay := time.Until(certificate.NotBefore.Add(time.Second))
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("await Mailpit certificate validity: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func awaitCapturedBody(ctx context.Context, endpoint, expected string) error {
	client := http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return fmt.Errorf("construct Mailpit request: %w", err)
		}
		response, err := client.Do(request)
		if err == nil {
			content, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			closeErr := response.Body.Close()
			if readErr == nil && closeErr == nil && response.StatusCode == http.StatusOK &&
				strings.Contains(string(content), expected) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("await Mailpit captured body: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func requireEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required for integration tests", name)
	}
	return value
}

func errorChain(err error) string {
	var chain []string
	for current := err; current != nil; current = errors.Unwrap(current) {
		chain = append(chain, fmt.Sprintf("%T: %v", current, current))
	}
	return strings.Join(chain, " -> ")
}
