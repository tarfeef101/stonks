package mail

import (
	"bufio"
	"context"
	"fmt"
	"html"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"finance-tracker/internal/config"
)

func assertNoExternalRefs(t *testing.T, label, body string) {
	t.Helper()
	for _, bad := range []string{"http://", "https://", "<a ", "<script", "<link"} {
		if strings.Contains(body, bad) {
			t.Errorf("%s body contains forbidden %q", label, bad)
		}
	}
}

func TestComposeWelcome(t *testing.T) {
	m, err := ComposeWelcome("eve@example.com", "Eve")
	if err != nil {
		t.Fatalf("ComposeWelcome: %v", err)
	}
	if m.To != "eve@example.com" {
		t.Errorf("To = %q", m.To)
	}
	if m.Subject != "Welcome to Personal Finance Tracker" {
		t.Errorf("Subject = %q", m.Subject)
	}
	if m.Text == "" {
		t.Error("Text is empty")
	}
	if !strings.Contains(m.Text, "Hi Eve,") {
		t.Errorf("Text missing display name: %q", m.Text)
	}
	if !strings.Contains(m.HTML, "Hi Eve,") {
		t.Errorf("HTML missing display name: %q", m.HTML)
	}
	if !strings.Contains(m.HTML, "<div") {
		t.Error("HTML has no inline-CSS container (<div)")
	}
	assertNoExternalRefs(t, "Text", m.Text)
	assertNoExternalRefs(t, "HTML", m.HTML)
	if len(m.Text) >= 1024 || len(m.HTML) >= 1024 {
		t.Errorf("bodies too large: %d text / %d html bytes", len(m.Text), len(m.HTML))
	}
}

func TestComposeVerification(t *testing.T) {
	m, err := ComposeVerification("eve@example.com", "Eve")
	if err != nil {
		t.Fatalf("ComposeVerification: %v", err)
	}
	if m.Subject != "Your Personal Finance Tracker account is set up" {
		t.Errorf("Subject = %q", m.Subject)
	}
	if m.Text == "" {
		t.Error("Text is empty")
	}
	if !strings.Contains(m.Text, "Hi Eve,") {
		t.Errorf("Text missing display name: %q", m.Text)
	}
	if !strings.Contains(m.HTML, "Hi Eve,") {
		t.Errorf("HTML missing display name: %q", m.HTML)
	}
	if !strings.Contains(m.HTML, "<div") {
		t.Error("HTML has no inline-CSS container (<div)")
	}
	assertNoExternalRefs(t, "Text", m.Text)
	assertNoExternalRefs(t, "HTML", m.HTML)
	if len(m.Text) >= 1024 || len(m.HTML) >= 1024 {
		t.Errorf("bodies too large: %d text / %d html bytes", len(m.Text), len(m.HTML))
	}
}

func TestComposeEmptyRecipient(t *testing.T) {
	if _, err := ComposeWelcome("", "Eve"); err == nil {
		t.Error("ComposeWelcome(\"\") should fail")
	}
	if _, err := ComposeVerification("", "Eve"); err == nil {
		t.Error("ComposeVerification(\"\") should fail")
	}
}

func TestComposeEmptyNameFallsBackToThere(t *testing.T) {
	for _, compose := range []struct {
		name    string
		compose func(to, name string) (Message, error)
	}{
		{"welcome", ComposeWelcome},
		{"verification", ComposeVerification},
	} {
		m, err := compose.compose("eve@example.com", "   ")
		if err != nil {
			t.Fatalf("%s: %v", compose.name, err)
		}
		if !strings.Contains(m.Text, "Hi there,") {
			t.Errorf("%s Text missing \"Hi there,\": %q", compose.name, m.Text)
		}
		if !strings.Contains(m.HTML, "Hi there,") {
			t.Errorf("%s HTML missing \"Hi there,\": %q", compose.name, m.HTML)
		}
	}
}

func TestComposeEscapesUntrustedName(t *testing.T) {
	evil := "<script>alert(1)</script>"
	m, err := ComposeWelcome("eve@example.com", evil)
	if err != nil {
		t.Fatalf("ComposeWelcome: %v", err)
	}
	if strings.Contains(m.HTML, "<script") {
		t.Error("HTML body contains unescaped script tag")
	}
	if !strings.Contains(m.HTML, html.EscapeString(evil)) {
		t.Errorf("HTML body missing escaped name; got %q", m.HTML)
	}
}

func TestNopSender(t *testing.T) {
	s := New(config.SMTP{})
	if _, ok := s.(NopSender); !ok {
		t.Fatalf("New(SMTP{}) = %T, want NopSender", s)
	}
	m, err := ComposeWelcome("eve@example.com", "Eve")
	if err != nil {
		t.Fatalf("ComposeWelcome: %v", err)
	}
	if err := s.Send(context.Background(), m); err != nil {
		t.Fatalf("NopSender.Send: %v", err)
	}
}

func TestNewRejectsUnsupportedPolicy(t *testing.T) {
	s := New(config.SMTP{Host: "smtp.example.com", Port: 25, TLSPolicy: "bogus"})
	m, _ := ComposeWelcome("eve@example.com", "Eve")
	if err := s.Send(context.Background(), m); err == nil {
		t.Error("want error for unsupported TLS policy")
	}
}

func TestSendDialFailure(t *testing.T) {
	s := New(config.SMTP{Host: "127.0.0.1", Port: 1, TLSPolicy: "none"})
	m, _ := ComposeWelcome("eve@example.com", "Eve")
	if err := s.Send(context.Background(), m); err == nil {
		t.Error("want dial error on closed port")
	}
}

// fakeSMTP is a minimal in-test SMTP server: line-based script
// (220 greet; 250 EHLO/MAIL/RCPT; 354 DATA then collect until ".";
// 221 QUIT) capturing the command sequence and full DATA payloads.
type fakeSMTP struct {
	ln net.Listener

	mu    sync.Mutex
	cmds  []string
	datas []string
}

func startFakeSMTP(t *testing.T) (*fakeSMTP, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake SMTP listen: %v", err)
	}
	f := &fakeSMTP{ln: ln}
	go f.acceptLoop()
	t.Cleanup(func() { ln.Close() })
	return f, ln.Addr().String()
}

func (f *fakeSMTP) acceptLoop() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeSMTP) handle(conn net.Conn) {
	defer conn.Close()
	write := func(line string) { fmt.Fprint(conn, line+"\r\n") }
	r := bufio.NewReader(conn)
	write("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		f.mu.Lock()
		f.cmds = append(f.cmds, cmd)
		f.mu.Unlock()
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			write("250-fake")
			write("250 OK")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			write("250 OK")
		case strings.HasPrefix(cmd, "RCPT TO"):
			write("250 OK")
		case cmd == "DATA":
			write("354 end data with <CR><LF>.<CR><LF>")
			var body []string
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}
				body = append(body, strings.TrimRight(l, "\r\n"))
			}
			f.mu.Lock()
			f.datas = append(f.datas, strings.Join(body, "\n"))
			f.mu.Unlock()
			write("250 OK")
		case strings.HasPrefix(cmd, "QUIT"):
			write("221 bye")
			return
		default:
			write("250 OK")
		}
	}
}

func (f *fakeSMTP) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func (f *fakeSMTP) data() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.datas) != 1 {
		return "", false
	}
	return f.datas[0], true
}

func TestSendPolicyNoneNoAuthDefaultFrom(t *testing.T) {
	f, addr := startFakeSMTP(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("Atoi: %v", err)
	}
	s := New(config.SMTP{Host: host, Port: p, TLSPolicy: "none"})
	m, err := ComposeWelcome("eve@example.com", "Eve")
	if err != nil {
		t.Fatalf("ComposeWelcome: %v", err)
	}
	if err := s.Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}

	cmds := f.commands()
	for _, c := range cmds {
		if strings.HasPrefix(c, "AUTH") {
			t.Errorf("unexpected AUTH %q (no user configured)", c)
		}
	}
	// net/smtp wraps the address in <>: the raw configured From is a
	// "Name <addr>" pair, so the wire line nests both.
	wantMail := "MAIL FROM:<Personal Finance Tracker <finance-tracker@localhost>>"
	if !hasCommand(cmds, wantMail) {
		t.Errorf("missing %q; got %q", wantMail, cmds)
	}
	if !hasCommand(cmds, "RCPT TO:<eve@example.com>") {
		t.Errorf("missing RCPT TO; got %q", cmds)
	}
	data, ok := f.data()
	if !ok {
		t.Fatalf("want exactly 1 DATA payload, got %d", f.dataCount())
	}
	// enmime renders a display-name From with quoted name and a
	// nameless To with angle brackets (RFC 5322).
	for _, want := range []string{
		"multipart/alternative",
		"text/plain",
		"text/html",
		"Subject: Welcome to Personal Finance Tracker",
		`From: "Personal Finance Tracker" <finance-tracker@localhost>`,
		"To: <eve@example.com>",
		"Hi Eve,",
		"account was created",
	} {
		if !strings.Contains(data, want) {
			t.Errorf("DATA missing %q", want)
		}
	}
}

func TestSendExplicitFrom(t *testing.T) {
	f, addr := startFakeSMTP(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	p, _ := strconv.Atoi(port)
	s := New(config.SMTP{Host: host, Port: p, TLSPolicy: "none", From: "finance@example.com"})
	m, err := ComposeVerification("eve@example.com", "Eve")
	if err != nil {
		t.Fatalf("ComposeVerification: %v", err)
	}
	if err := s.Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	cmds := f.commands()
	if !hasCommand(cmds, "MAIL FROM:<finance@example.com>") {
		t.Errorf("missing MAIL FROM; got %q", cmds)
	}
	data, ok := f.data()
	if !ok {
		t.Fatalf("want exactly 1 DATA payload")
	}
	for _, want := range []string{
		"Subject: Your Personal Finance Tracker account is set up",
		"From: <finance@example.com>", // bare address, no display name
		"To: <eve@example.com>",
		"set up and ready",
	} {
		if !strings.Contains(data, want) {
			t.Errorf("DATA missing %q", want)
		}
	}
}

func (f *fakeSMTP) dataCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.datas)
}

func hasCommand(cmds []string, want string) bool {
	for _, c := range cmds {
		if c == want {
			return true
		}
	}
	return false
}
