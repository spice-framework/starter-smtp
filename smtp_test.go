package smtp_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spicemail "github.com/StevenBuglione/spice/mail"
	"github.com/StevenBuglione/spice/starter/smtp"
)

const (
	testServerName = "smtp.example.test"
	testUsername   = "mailer"
	testPassword   = "correct horse battery staple"
)

func TestStartTLSSenderDeliversExactMIMEWithAuthentication(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, serverOptions{
		mode:        smtp.TLSModeStartTLS,
		requireAuth: true,
	})
	var observations []smtp.Observation
	sender, err := smtp.New(smtp.Config{
		Address:    server.address(),
		ServerName: testServerName,
		Username:   testUsername,
		Password:   testPassword,
		TLSConfig:  server.clientTLSConfig(),
		Observer: func(_ context.Context, observation smtp.Observation) {
			observations = append(observations, observation)
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	message := testMessage(t, "starttls@example.test")
	if err := sender.Send(context.Background(), message); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	delivered := server.nextMessage(t)
	if !bytes.Equal(delivered, message.Bytes()) {
		t.Fatalf("delivered MIME differs:\n%s", delivered)
	}
	if !server.authenticated() {
		t.Fatal("server did not observe authentication after STARTTLS")
	}
	if len(observations) != 1 ||
		observations[0].Outcome != smtp.OutcomeDelivered ||
		observations[0].MessageID != message.ID() ||
		observations[0].Attempt != 1 ||
		observations[0].MaxAttempts != 1 {
		t.Fatalf("observations = %#v", observations)
	}
	assertPayloadFree(t, fmt.Sprintf("%#v", observations))
}

func TestImplicitTLSSenderDeliversConcurrently(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, serverOptions{mode: smtp.TLSModeImplicitTLS})
	sender, err := smtp.New(smtp.Config{
		Address:    server.address(),
		ServerName: testServerName,
		Mode:       smtp.TLSModeImplicitTLS,
		TLSConfig:  server.clientTLSConfig(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	const sends = 8
	errs := make(chan error, sends)
	for index := range sends {
		go func() {
			message := testMessage(t, fmt.Sprintf("concurrent-%d@example.test", index))
			errs <- sender.Send(context.Background(), message)
		}()
	}
	for range sends {
		if err := <-errs; err != nil {
			t.Fatalf("Send() error = %v", err)
		}
	}
	if got := server.deliveryCount(); got != sends {
		t.Fatalf("delivery count = %d, want %d", got, sends)
	}
}

func TestSenderRetriesOnlyTemporaryPreDataFailures(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, serverOptions{
		mode:               smtp.TLSModeStartTLS,
		temporaryMailFails: 1,
	})
	var (
		delays       []time.Duration
		observations []smtp.Observation
	)
	sender, err := smtp.New(smtp.Config{
		Address:        server.address(),
		ServerName:     testServerName,
		TLSConfig:      server.clientTLSConfig(),
		MaxAttempts:    2,
		InitialBackoff: 25 * time.Millisecond,
		MaxBackoff:     25 * time.Millisecond,
		Wait: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
		Observer: func(_ context.Context, observation smtp.Observation) {
			observations = append(observations, observation)
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := sender.Send(context.Background(), testMessage(t, "retry@example.test")); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if !slices.Equal(delays, []time.Duration{25 * time.Millisecond}) {
		t.Fatalf("retry delays = %v", delays)
	}
	if server.connectionCount() != 2 || server.deliveryCount() != 1 {
		t.Fatalf(
			"connections = %d, deliveries = %d",
			server.connectionCount(),
			server.deliveryCount(),
		)
	}
	if len(observations) != 2 ||
		observations[0].Outcome != smtp.OutcomeRetrying ||
		observations[0].Stage != smtp.StageEnvelopeFrom ||
		observations[0].Code != 451 ||
		!observations[0].Temporary ||
		observations[0].NextBackoff != 25*time.Millisecond ||
		observations[1].Outcome != smtp.OutcomeDelivered {
		t.Fatalf("observations = %#v", observations)
	}
}

func TestSenderNeverRetriesAmbiguousPostDataFailure(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, serverOptions{
		mode:          smtp.TLSModeStartTLS,
		dropAfterData: true,
	})
	var observations []smtp.Observation
	waitCalled := false
	sender, err := smtp.New(smtp.Config{
		Address:        server.address(),
		ServerName:     testServerName,
		TLSConfig:      server.clientTLSConfig(),
		MaxAttempts:    3,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		Wait: func(context.Context, time.Duration) error {
			waitCalled = true
			return nil
		},
		Observer: func(_ context.Context, observation smtp.Observation) {
			observations = append(observations, observation)
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	err = sender.Send(context.Background(), testMessage(t, "ambiguous@example.test"))
	delivery, ok := errors.AsType[*smtp.DeliveryError](err)
	if !ok ||
		delivery.Stage() != smtp.StageAcceptance ||
		delivery.RetrySafe() {
		t.Fatalf("Send() error = %#v", err)
	}
	if waitCalled || server.connectionCount() != 1 || server.deliveryCount() != 1 {
		t.Fatalf(
			"wait = %t, connections = %d, accepted data = %d",
			waitCalled,
			server.connectionCount(),
			server.deliveryCount(),
		)
	}
	if len(observations) != 1 ||
		observations[0].Outcome != smtp.OutcomeFailed ||
		observations[0].Stage != smtp.StageAcceptance {
		t.Fatalf("observations = %#v", observations)
	}
}

func TestSenderFailsClosedWithoutSTARTTLS(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, serverOptions{
		mode:               smtp.TLSModeStartTLS,
		disableStartTLS:    true,
		temporaryMailFails: 0,
	})
	sender, err := smtp.New(smtp.Config{
		Address:    server.address(),
		ServerName: testServerName,
		TLSConfig:  server.clientTLSConfig(),
		Username:   testUsername,
		Password:   testPassword,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	err = sender.Send(context.Background(), testMessage(t, "no-tls@example.test"))
	delivery, ok := errors.AsType[*smtp.DeliveryError](err)
	if !ok ||
		delivery.Stage() != smtp.StageStartTLS ||
		delivery.Temporary() ||
		server.authenticated() ||
		server.deliveryCount() != 0 {
		t.Fatalf("Send() error = %#v", err)
	}
}

func TestSenderHonorsCancellationAndTimeoutBeforeGreeting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		timeout time.Duration
		cancel  bool
		want    error
	}{
		{
			name:    "caller cancellation",
			timeout: time.Second,
			cancel:  true,
			want:    context.Canceled,
		},
		{
			name:    "configured timeout",
			timeout: 30 * time.Millisecond,
			want:    context.DeadlineExceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := newTestServer(t, serverOptions{
				mode:          smtp.TLSModeStartTLS,
				stallGreeting: true,
			})
			sender, err := smtp.New(smtp.Config{
				Address:    server.address(),
				ServerName: testServerName,
				TLSConfig:  server.clientTLSConfig(),
				Timeout:    test.timeout,
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.cancel {
				go func() {
					<-server.accepted
					cancel()
				}()
			}
			err = sender.Send(ctx, testMessage(t, "cancel@example.test"))
			if !errors.Is(err, test.want) {
				t.Fatalf("Send() error = %v, want %v", err, test.want)
			}
			delivery, ok := errors.AsType[*smtp.DeliveryError](err)
			if !ok || delivery.Stage() != smtp.StageGreeting {
				t.Fatalf("Send() error = %#v", err)
			}
		})
	}
}

func TestConfigValidationAndSanitizedErrors(t *testing.T) {
	t.Parallel()
	valid := smtp.Config{
		Address:    "127.0.0.1:2525",
		ServerName: testServerName,
	}
	tests := []struct {
		name   string
		mutate func(*smtp.Config)
	}{
		{"missing address", func(config *smtp.Config) { config.Address = "" }},
		{"URL address", func(config *smtp.Config) { config.Address = "smtp://localhost:25" }},
		{"missing port", func(config *smtp.Config) { config.Address = "localhost" }},
		{"zero port", func(config *smtp.Config) { config.Address = "localhost:0" }},
		{"invalid mode", func(config *smtp.Config) { config.Mode = "cleartext" }},
		{"username only", func(config *smtp.Config) { config.Username = "mailer" }},
		{"password only", func(config *smtp.Config) { config.Password = testPassword }},
		{"negative timeout", func(config *smtp.Config) { config.Timeout = -1 }},
		{"excessive timeout", func(config *smtp.Config) { config.Timeout = 3 * time.Minute }},
		{"negative backoff", func(config *smtp.Config) { config.InitialBackoff = -1 }},
		{"reversed backoff", func(config *smtp.Config) {
			config.InitialBackoff = time.Second
		}},
		{"invalid multiplier", func(config *smtp.Config) { config.Multiplier = 1 }},
		{"too many attempts", func(config *smtp.Config) { config.MaxAttempts = 11 }},
		{"insecure TLS", func(config *smtp.Config) {
			config.TLSConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // Proves rejection.
		}},
		{"old TLS", func(config *smtp.Config) {
			config.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS11}
		}},
		{"renegotiation", func(config *smtp.Config) {
			config.TLSConfig = &tls.Config{
				Renegotiation: tls.RenegotiateFreelyAsClient,
			}
		}},
		{"conflicting name", func(config *smtp.Config) {
			config.TLSConfig = &tls.Config{ServerName: "other.example.test"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := valid
			test.mutate(&config)
			if _, err := smtp.New(config); err == nil {
				t.Fatal("New() error = nil")
			}
		})
	}

	unused, err := smtp.New(valid)
	if err != nil || unused == nil {
		t.Fatalf("New(valid unused address) = %#v, %v", unused, err)
	}
	if err := unused.Send(context.Background(), spicemail.Message{}); err == nil {
		t.Fatal("Send(zero message) error = nil")
	}
	var nilSender *smtp.Sender
	if err := nilSender.Send(context.Background(), testMessage(t, "nil@example.test")); err == nil {
		t.Fatal("nil Sender.Send() error = nil")
	}
	if err := unused.Send(nilContext(), testMessage(t, "nil-context@example.test")); err == nil {
		t.Fatal("Send(nil context) error = nil")
	}
}

func TestDeliveryErrorDoesNotExposeSecretsOrServerText(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, serverOptions{
		mode:               smtp.TLSModeStartTLS,
		temporaryMailFails: 1,
		failureText:        "secret-token customer@example.test",
	})
	sender, err := smtp.New(smtp.Config{
		Address:    server.address(),
		ServerName: testServerName,
		TLSConfig:  server.clientTLSConfig(),
		Username:   testUsername,
		Password:   testPassword,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	err = sender.Send(context.Background(), testMessage(t, "safe-error@example.test"))
	if err == nil {
		t.Fatal("Send() error = nil")
	}
	assertPayloadFree(t, err.Error())
	delivery, ok := errors.AsType[*smtp.DeliveryError](err)
	if !ok ||
		delivery.Code() != 451 ||
		!delivery.Temporary() ||
		!delivery.RetrySafe() {
		t.Fatalf("Send() error = %#v", err)
	}
	if (*smtp.DeliveryError)(nil).Error() != "SMTP delivery failed" ||
		(*smtp.DeliveryError)(nil).Unwrap() != nil ||
		(*smtp.DeliveryError)(nil).Stage() != "" ||
		(*smtp.DeliveryError)(nil).Code() != 0 ||
		(*smtp.DeliveryError)(nil).Temporary() ||
		(*smtp.DeliveryError)(nil).RetrySafe() {
		t.Fatal("nil DeliveryError contract changed")
	}
}

func testMessage(t *testing.T, id string) spicemail.Message {
	t.Helper()
	message, err := spicemail.NewMessage(spicemail.MessageSpec{
		ID:       id,
		Date:     time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC),
		From:     "Orders <orders@example.test>",
		To:       []string{"customer@example.test"},
		Bcc:      []string{"audit@example.test"},
		Subject:  "Order secret subject",
		TextBody: "private text body",
		HTMLBody: "<p>private HTML body</p>",
		Attachments: []spicemail.AttachmentSpec{{
			Filename:    "receipt.txt",
			ContentType: "text/plain; charset=utf-8",
			Data:        []byte("private attachment"),
		}},
	})
	if err != nil {
		t.Fatalf("mail.NewMessage() error = %v", err)
	}
	return message
}

func assertPayloadFree(t *testing.T, value string) {
	t.Helper()
	for _, secret := range []string{
		testUsername,
		testPassword,
		"secret-token",
		"customer@example.test",
		"audit@example.test",
		"Order secret subject",
		"private text body",
		"private HTML body",
		"private attachment",
	} {
		if strings.Contains(value, secret) {
			t.Fatalf("value %q contains secret %q", value, secret)
		}
	}
}

func nilContext() context.Context {
	return nil
}

type serverOptions struct {
	mode               smtp.TLSMode
	requireAuth        bool
	disableStartTLS    bool
	temporaryMailFails int
	dropAfterData      bool
	stallGreeting      bool
	failureText        string
}

type testSMTPServer struct {
	t              *testing.T
	listener       net.Listener
	options        serverOptions
	tlsConfig      *tls.Config
	roots          *x509.CertPool
	accepted       chan struct{}
	messages       chan []byte
	done           chan struct{}
	connections    atomic.Int64
	authCount      atomic.Int64
	deliveryNumber atomic.Int64
	wait           sync.WaitGroup
}

func newTestServer(t *testing.T, options serverOptions) *testSMTPServer {
	t.Helper()
	certificate, roots := testCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	server := &testSMTPServer{
		t:        t,
		listener: listener,
		options:  options,
		tlsConfig: &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
		},
		roots:    roots,
		accepted: make(chan struct{}, 32),
		messages: make(chan []byte, 32),
		done:     make(chan struct{}),
	}
	server.wait.Go(server.accept)
	t.Cleanup(func() {
		close(server.done)
		closeTestConnection(server.listener)
		server.wait.Wait()
	})
	return server
}

func (server *testSMTPServer) address() string {
	return server.listener.Addr().String()
}

func (server *testSMTPServer) clientTLSConfig() *tls.Config {
	return &tls.Config{
		RootCAs:    server.roots,
		MinVersion: tls.VersionTLS12,
	}
}

func (server *testSMTPServer) connectionCount() int {
	return int(server.connections.Load())
}

func (server *testSMTPServer) deliveryCount() int {
	return int(server.deliveryNumber.Load())
}

func (server *testSMTPServer) authenticated() bool {
	return server.authCount.Load() > 0
}

func (server *testSMTPServer) nextMessage(t *testing.T) []byte {
	t.Helper()
	select {
	case message := <-server.messages:
		return message
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delivered message")
		return nil
	}
}

func (server *testSMTPServer) accept() {
	for {
		connection, err := server.listener.Accept()
		if err != nil {
			select {
			case <-server.done:
				return
			default:
				server.t.Errorf("SMTP test server accept: %v", err)
				return
			}
		}
		attempt := int(server.connections.Add(1))
		select {
		case server.accepted <- struct{}{}:
		default:
		}
		server.wait.Go(func() {
			server.serve(connection, attempt)
		})
	}
}

func (server *testSMTPServer) serve(connection net.Conn, attempt int) {
	defer closeTestConnection(connection)
	if server.options.mode == smtp.TLSModeImplicitTLS {
		secured := tls.Server(connection, server.tlsConfig.Clone())
		if err := secured.Handshake(); err != nil {
			return
		}
		connection = secured
	}
	if server.options.stallGreeting {
		buffer := make([]byte, 1)
		if _, readErr := connection.Read(buffer); readErr != nil {
			return
		}
		return
	}
	reader := bufio.NewReader(connection)
	writer := bufio.NewWriter(connection)
	if !writeResponse(writer, "220 test ESMTP ready\r\n") {
		return
	}
	secure := server.options.mode == smtp.TLSModeImplicitTLS
	authenticated := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command, argument := splitCommand(line)
		switch command {
		case "EHLO", "HELO":
			switch {
			case secure && server.options.requireAuth:
				if !writeResponse(writer, "250-test\r\n250 AUTH PLAIN\r\n") {
					return
				}
			case !secure &&
				server.options.mode == smtp.TLSModeStartTLS &&
				!server.options.disableStartTLS:
				if !writeResponse(writer, "250-test\r\n250 STARTTLS\r\n") {
					return
				}
			default:
				if !writeResponse(writer, "250 test\r\n") {
					return
				}
			}
		case "STARTTLS":
			if secure || server.options.disableStartTLS {
				if !writeResponse(writer, "454 TLS unavailable\r\n") {
					return
				}
				continue
			}
			if !writeResponse(writer, "220 begin TLS\r\n") {
				return
			}
			secured := tls.Server(connection, server.tlsConfig.Clone())
			if err := secured.Handshake(); err != nil {
				return
			}
			connection = secured
			reader = bufio.NewReader(connection)
			writer = bufio.NewWriter(connection)
			secure = true
		case "AUTH":
			if !secure || !validPlainAuth(argument) {
				if !writeResponse(writer, "535 authentication failed\r\n") {
					return
				}
				continue
			}
			authenticated = true
			server.authCount.Add(1)
			if !writeResponse(writer, "235 authenticated\r\n") {
				return
			}
		case "MAIL":
			if server.options.requireAuth && !authenticated {
				if !writeResponse(writer, "530 authentication required\r\n") {
					return
				}
				continue
			}
			if attempt <= server.options.temporaryMailFails {
				text := server.options.failureText
				if text == "" {
					text = "temporary unavailable"
				}
				_ = writeResponse(writer, "451 "+text+"\r\n")
				return
			}
			if !writeResponse(writer, "250 sender accepted\r\n") {
				return
			}
		case "RCPT":
			if !writeResponse(writer, "250 recipient accepted\r\n") {
				return
			}
		case "DATA":
			if !writeResponse(writer, "354 send data\r\n") {
				return
			}
			message, readErr := readMessageData(reader)
			if readErr != nil {
				return
			}
			server.deliveryNumber.Add(1)
			server.messages <- message
			if server.options.dropAfterData {
				return
			}
			if !writeResponse(writer, "250 accepted\r\n") {
				return
			}
		case "QUIT":
			_ = writeResponse(writer, "221 goodbye\r\n")
			return
		default:
			if !writeResponse(writer, "500 unsupported\r\n") {
				return
			}
		}
	}
}

func splitCommand(line string) (string, string) {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	command, argument, found := strings.Cut(trimmed, " ")
	if !found {
		return strings.ToUpper(command), ""
	}
	return strings.ToUpper(command), argument
}

func validPlainAuth(argument string) bool {
	mechanism, encoded, found := strings.Cut(argument, " ")
	if !found || !strings.EqualFold(mechanism, "PLAIN") {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	return bytes.Equal(
		decoded,
		[]byte("\x00"+testUsername+"\x00"+testPassword),
	)
}

func writeResponse(writer *bufio.Writer, response string) bool {
	if _, err := writer.WriteString(response); err != nil {
		return false
	}
	return writer.Flush() == nil
}

func closeTestConnection(closer interface{ Close() error }) {
	if closeErr := closer.Close(); closeErr != nil {
		return
	}
}

func readMessageData(reader *bufio.Reader) ([]byte, error) {
	var content bytes.Buffer
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == ".\r\n" {
			return content.Bytes(), nil
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		content.WriteString(line)
	}
}

func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey() error = %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: testServerName},
		DNSNames:     []string{testServerName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatalf("x509.CreateCertificate() error = %v", err)
	}
	certificate, err := tls.X509KeyPair(
		pemEncode("CERTIFICATE", der),
		pemEncode("PRIVATE KEY", mustPKCS8(t, private)),
	)
	if err != nil {
		t.Fatalf("tls.X509KeyPair() error = %v", err)
	}
	roots := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("x509.ParseCertificate() error = %v", err)
	}
	roots.AddCert(parsed)
	return certificate, roots
}

func mustPKCS8(t *testing.T, private ed25519.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatalf("x509.MarshalPKCS8PrivateKey() error = %v", err)
	}
	return der
}

func pemEncode(kind string, der []byte) []byte {
	encoded := base64.StdEncoding.EncodeToString(der)
	var content strings.Builder
	content.WriteString("-----BEGIN ")
	content.WriteString(kind)
	content.WriteString("-----\n")
	for len(encoded) > 64 {
		content.WriteString(encoded[:64])
		content.WriteByte('\n')
		encoded = encoded[64:]
	}
	content.WriteString(encoded)
	content.WriteString("\n-----END ")
	content.WriteString(kind)
	content.WriteString("-----\n")
	return []byte(content.String())
}
