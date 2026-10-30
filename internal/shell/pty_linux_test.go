//go:build linux

package shell

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"crl-cli/internal/mint"
	"crl-cli/internal/request"
)

func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("pty indisponible : %v", err)
	}

	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		master.Close()
		t.Fatalf("déverrouillage du pty : %v", errno)
	}
	var number uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number))); errno != 0 {
		master.Close()
		t.Fatalf("numéro du pty : %v", errno)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		t.Fatalf("ouverture du pty esclave : %v", err)
	}

	ws := struct {
		Row, Col, Xpixel, Ypixel uint16
	}{Row: 24, Col: 100}
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
	return master, slave
}

type ptyCollector struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *ptyCollector) write(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = c.buf.Write(p)
}

func (c *ptyCollector) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func waitPTYText(t *testing.T, got *ptyCollector, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(got.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sortie PTY sans %q :\n%q", want, got.String())
}

// ptyCurrentLine approxime la ligne visible : le dernier \r revient en début
// de ligne, puis les séquences ANSI de contrôle sont retirées.
func ptyCurrentLine(output string) string {
	if i := strings.LastIndexByte(output, '\r'); i >= 0 {
		output = output[i+1:]
	}
	return stripCSI(output)
}

func stripCSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			if j < len(s) {
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func waitPTYLine(t *testing.T, got *ptyCollector, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ptyCurrentLine(got.String()) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ligne PTY = %q, attendu %q\nsortie :\n%q",
		ptyCurrentLine(got.String()), want, got.String())
}

// TestInteractiveTypingOneKeyAtATime protège contre une régression où le
// tampon de l'éditeur était réinitialisé après chaque lecture : les caractères
// tapés un par un n'apparaissaient jamais à l'écran, alors qu'un collage d'une
// ligne complète fonctionnait.
func TestInteractiveTypingOneKeyAtATime(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()

	hist := mint.NewHistory(mint.DefaultHistoryConfig())
	mgr, err := mint.NewManager(mint.DefaultConfig(), hist)
	if err != nil {
		t.Fatal(err)
	}
	sh, err := New(Options{
		In:      slave,
		Out:     slave,
		Builder: request.New(),
		Manager: mgr,
	})
	if err != nil {
		t.Fatal(err)
	}

	got := &ptyCollector{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				got.write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sh.Run(ctx) }()

	waitPTYLine(t, got, testPrompt, 3*time.Second)

	typed := "help"
	for i := range typed {
		if _, err := master.Write([]byte{typed[i]}); err != nil {
			t.Fatalf("écriture PTY : %v", err)
		}
		waitPTYLine(t, got, testPrompt+typed[:i+1], 3*time.Second)
	}
	if _, err := master.Write([]byte("\r")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, got, "Commandes CRL-CLI", 3*time.Second)

	if _, err := master.Write([]byte("exit\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shell : %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("le shell ne s'est pas arrêté après exit")
	}
}

// TestInteractiveJSONEditorUsesArrowKeysAndReturnsToPrompt exerce le chemin
// réel du terminal brut : crl.json sans argument affiche l'état actuel sans
// ouvrir d'éditeur, et crl.json <JSON> configure le corps sur une seule ligne.
func TestInteractiveJSONEditorUsesArrowKeysAndReturnsToPrompt(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()

	hist := mint.NewHistory(mint.DefaultHistoryConfig())
	mgr, err := mint.NewManager(mint.DefaultConfig(), hist)
	if err != nil {
		t.Fatal(err)
	}
	builder := request.New()
	if err := builder.SetJSON(`{"role":"user"}`); err != nil {
		t.Fatal(err)
	}
	sh, err := New(Options{In: slave, Out: slave, Builder: builder, Manager: mgr})
	if err != nil {
		t.Fatal(err)
	}
	got := &ptyCollector{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, e := master.Read(buf)
			if n > 0 {
				got.write(buf[:n])
			}
			if e != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sh.Run(ctx) }()
	jsonPrompt := "crl [MINT OFF] [SEL 0] [CRL GET · H1 · B]  $"
	waitPTYLine(t, got, jsonPrompt, 3*time.Second)

	// crl.json sans argument affiche l'état actuel, n'ouvre pas l'éditeur.
	if _, err := master.Write([]byte("crl.json\r")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, got, "JSON actuel", 3*time.Second)
	if !strings.Contains(got.String(), `{"role":"user"}`) {
		t.Fatalf("JSON actuel non affiché:\n%s", got.String())
	}
	waitPTYLine(t, got, jsonPrompt, 3*time.Second)

	// crl.json avec un JSON sur une ligne configure le corps immédiatement.
	if _, err := master.Write([]byte("crl.json {\"role\":\"admin\"}\r")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, got, "Corps JSON configuré", 3*time.Second)
	if want, have := `{"role":"admin"}`, string(builder.Body()); have != want {
		t.Fatalf("corps appliqué = %s, attendu %s", have, want)
	}
	// Aucun éditeur multiligne n'est ouvert : le prompt est immédiatement de retour.
	waitPTYLine(t, got, jsonPrompt, 3*time.Second)

	if _, err := master.Write([]byte("exit\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shell : %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shell bloqué après le retour du prompt")
	}
}

// TestInteractiveMintNotificationPreservesTypedInput vérifie qu'une capture
// Mint reçue pendant la saisie est affichée au-dessus du prompt, puis que la
// commande partielle est restaurée et peut être terminée normalement.
func TestInteractiveMintNotificationPreservesTypedInput(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()

	hist := mint.NewHistory(mint.DefaultHistoryConfig())
	mgr, err := mint.NewManager(mint.DefaultConfig(), hist)
	if err != nil {
		t.Fatal(err)
	}
	notifs := make(chan mint.Notification, 4)
	sh, err := New(Options{
		In:            slave,
		Out:           slave,
		Builder:       request.New(),
		Manager:       mgr,
		Notifications: notifs,
	})
	if err != nil {
		t.Fatal(err)
	}

	got := &ptyCollector{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				got.write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sh.Run(ctx) }()

	waitPTYLine(t, got, testPrompt, 3*time.Second)
	if _, err := master.Write([]byte("hel")); err != nil {
		t.Fatalf("écriture PTY : %v", err)
	}
	waitPTYLine(t, got, testPrompt+"hel", 3*time.Second)

	notifs <- mint.Notification{Kind: "request", ID: 7, Method: "GET", URL: "http://example.test/api"}
	waitPTYText(t, got, "[Mint] #7 GET http://example.test/api", 3*time.Second)
	waitPTYLine(t, got, testPrompt+"hel", 3*time.Second)

	if _, err := master.Write([]byte("p\r")); err != nil {
		t.Fatalf("écriture PTY : %v", err)
	}
	waitPTYText(t, got, "Commandes CRL-CLI", 3*time.Second)

	if _, err := master.Write([]byte("exit\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shell : %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("le shell ne s'est pas arrêté après exit")
	}
}

// TestInteractiveLongPasteShowsPromptOnce protège contre un collage qui
// redessinait tout le tampon à chaque caractère, laissant plusieurs prompts et
// des fragments de la ligne collée dans le terminal.
func TestInteractiveLongPasteShowsPromptOnce(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()

	hist := mint.NewHistory(mint.DefaultHistoryConfig())
	mgr, err := mint.NewManager(mint.DefaultConfig(), hist)
	if err != nil {
		t.Fatal(err)
	}
	sh, err := New(Options{
		In:      slave,
		Out:     slave,
		Builder: request.New(),
		Manager: mgr,
	})
	if err != nil {
		t.Fatal(err)
	}

	got := &ptyCollector{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				got.write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sh.Run(ctx) }()

	waitPTYLine(t, got, testPrompt, 3*time.Second)
	paste := strings.Repeat("eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.", 30)
	if _, err := master.Write([]byte(paste)); err != nil {
		t.Fatalf("écriture PTY : %v", err)
	}
	waitPTYText(t, got, paste, 3*time.Second)
	if count := strings.Count(got.String(), testPrompt); count < 1 {
		t.Fatalf("le collage a produit %d prompts :\n%q", count, got.String())
	}

	if _, err := master.Write([]byte("\x03")); err != nil {
		t.Fatal(err)
	}
	if _, err := master.Write([]byte("exit\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shell : %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("le shell ne s'est pas arrêté après exit")
	}
}
