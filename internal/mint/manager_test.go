package mint

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	requireLoopbackTCP(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Port = freePort(t)
	cfg.StartTimeout = 5 * time.Second
	cfg.StopGrace = 2 * time.Second
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.MitmdumpPath = exe
	return cfg
}

func testManager(t *testing.T, cfg Config, mode, marker string) (*Manager, *History) {
	t.Helper()
	hist := NewHistory(HistoryConfig{
		MaxCaptures:       cfg.MaxCaptures,
		MaxBodyBytes:      cfg.MaxBodyBytes,
		MaxTotalBodyBytes: cfg.MaxTotalBodyBytes,
	})
	m, err := NewManager(cfg, hist)
	if err != nil {
		t.Fatal(err)
	}
	m.commandBuilder = func(_ *Manager, _ string, _ string, _ *Bridge, _ Config) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=TestMintHelperProcess", "--")
		cmd.Env = append(os.Environ(),
			"GO_WANT_MINT_HELPER=1",
			"MINT_HELPER_MODE="+mode,
			"MINT_HELPER_MARKER="+marker,
		)
		return cmd, nil
	}
	return m, hist
}

func TestMintHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_MINT_HELPER") != "1" {
		return
	}
	mode := os.Getenv("MINT_HELPER_MODE")
	host := os.Getenv("MINT_BRIDGE_HOST")
	port := os.Getenv("MINT_BRIDGE_PORT")
	token := os.Getenv("MINT_BRIDGE_TOKEN")
	if host == "" || port == "" || token == "" {
		os.Exit(10)
	}
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	post := func(payload map[string]interface{}) bool {
		data, _ := json.Marshal(payload)
		req, err := http.NewRequest(http.MethodPost, "http://"+host+":"+port+"/events", bytes.NewReader(data))
		if err != nil {
			return false
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusNoContent
	}
	now := func() string { return time.Now().UTC().Format(time.RFC3339Nano) }
	writeMarker := func() {
		if marker := os.Getenv("MINT_HELPER_MARKER"); marker != "" {
			_ = os.WriteFile(marker, []byte("stopped"), 0o600)
		}
	}
	waitSignal := func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-ch:
			writeMarker()
			os.Exit(0)
		case <-time.After(8 * time.Second):
			os.Exit(7)
		}
	}

	switch mode {
	case "ready":
		if !post(map[string]interface{}{"type": "ready", "version": 1, "timestamp": now()}) {
			os.Exit(11)
		}
		waitSignal()
	case "capture":
		if !post(map[string]interface{}{"type": "ready", "version": 1, "timestamp": now()}) {
			os.Exit(11)
		}
		post(map[string]interface{}{
			"type":         "request",
			"flow_id":      "helper-flow",
			"timestamp":    now(),
			"method":       "POST",
			"url":          "http://127.0.0.1:5002/users/v1/login?next=%2Fhome&x=1&x=",
			"http_version": "HTTP/1.1",
			"headers": []map[string]string{
				{"name": "Authorization", "value": "Bearer super-secret"},
				{"name": "Content-Type", "value": "application/json"},
			},
			"body_b64":  "eyJ1c2VyIjoiZGVtbyJ9",
			"body_size": 15,
		})
		post(map[string]interface{}{
			"type":         "response",
			"flow_id":      "helper-flow",
			"timestamp":    now(),
			"status_code":  200,
			"reason":       "OK",
			"http_version": "HTTP/1.1",
			"headers": []map[string]string{
				{"name": "Set-Cookie", "value": "session=abc"},
				{"name": "Content-Type", "value": "application/json"},
			},
			"body_b64":    "eyJvayI6dHJ1ZX0=",
			"body_size":   11,
			"duration_ms": 12.5,
		})
		waitSignal()
	case "crash-after-ready":
		if !post(map[string]interface{}{"type": "ready", "version": 1, "timestamp": now()}) {
			os.Exit(11)
		}
		time.Sleep(150 * time.Millisecond)
		os.Exit(4)
	case "no-ready":
		time.Sleep(10 * time.Second)
		os.Exit(6)
	default:
		os.Exit(12)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func TestValidatePort(t *testing.T) {
	for _, port := range []int{0, -1, 65536, 70000} {
		if err := ValidatePort(port); err == nil {
			t.Fatalf("port %d should be invalid", port)
		}
	}
	for _, port := range []int{1, 80, 8080, 65535} {
		if err := ValidatePort(port); err != nil {
			t.Fatalf("port %d should be valid: %v", port, err)
		}
	}
}

func TestConfigRejectsRemoteListenHost(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ListenHost = "0.0.0.0"
	cfg.Port = 8080
	if err := cfg.Validate(); err == nil {
		t.Fatal("remote listen host should be rejected")
	}
	cfg.ListenHost = "127.0.0.1"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerMitmdumpMissing(t *testing.T) {
	cfg := testConfig(t)
	cfg.MitmdumpPath = "crl-cli-mitmdump-absent-for-test"
	hist := NewHistory(DefaultHistoryConfig())
	m, err := NewManager(cfg, hist)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "introuvable") {
		t.Fatalf("expected mitmdump missing error, got %v", err)
	}
	if m.Status().State != StateFailed {
		t.Fatalf("state = %s", m.Status().State)
	}
}

func TestManagerPortBusy(t *testing.T) {
	requireLoopbackTCP(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	cfg := DefaultConfig()
	cfg.Port = port
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.MitmdumpPath = exe
	hist := NewHistory(DefaultHistoryConfig())
	m, err := NewManager(cfg, hist)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "indisponible") {
		t.Fatalf("expected busy port error, got %v", err)
	}
	if m.Status().State != StateFailed {
		t.Fatalf("state = %s", m.Status().State)
	}
}

func TestManagerStartStopCapture(t *testing.T) {
	cfg := testConfig(t)
	marker := filepath.Join(t.TempDir(), "stopped")
	m, hist := testManager(t, cfg, "capture", marker)
	notifs := make(chan Notification, 32)
	m.SetNotifier(func(n Notification) { notifs <- n })

	info, err := m.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Listen != fmt.Sprintf("127.0.0.1:%d", cfg.Port) {
		t.Fatalf("listen = %q", info.Listen)
	}
	if st := m.Status(); st.State != StateRunning || !st.BridgeConnected {
		t.Fatalf("status = %#v", st)
	}

	waitFor(t, 3*time.Second, func() bool {
		req, resp := hist.Counts()
		return req == 1 && resp == 1
	})
	captures := hist.List()
	if len(captures) != 1 {
		t.Fatalf("captures = %d", len(captures))
	}
	c := captures[0]
	if c.Request.Method != "POST" || c.Request.Host != "127.0.0.1:5002" || c.Request.RawQuery != "next=%2Fhome&x=1&x=" {
		t.Fatalf("capture request = %#v", c.Request)
	}
	if c.Response == nil || c.Response.StatusCode != 200 || string(c.Response.Body) != `{"ok":true}` {
		t.Fatalf("capture response = %#v", c.Response)
	}
	if c.Duration != 12500*time.Microsecond {
		t.Fatalf("duration = %s", c.Duration)
	}
	if ok, reason := c.Importable(); !ok {
		t.Fatalf("not importable: %s", reason)
	}

	var sawRequest, sawResponse bool
	deadline := time.After(2 * time.Second)
	for !sawRequest || !sawResponse {
		select {
		case n := <-notifs:
			if n.Kind == "request" {
				sawRequest = true
				if strings.Contains(n.URL, "super-secret") || strings.Contains(n.URL, "token") {
					t.Fatalf("notification leaked secret: %q", n.URL)
				}
			}
			if n.Kind == "response" {
				sawResponse = true
			}
		case <-deadline:
			t.Fatalf("notifications missing request=%v response=%v", sawRequest, sawResponse)
		}
	}

	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(); st.State != StateStopped {
		t.Fatalf("state after stop = %s", st.State)
	}
	if st := m.Status(); st.BridgeConnected {
		t.Fatal("bridge still connected after stop")
	}
	if _, err := os.Stat(info.AddonPath); !os.IsNotExist(err) {
		t.Fatalf("addon temp file not removed: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("helper did not receive graceful signal: %v", err)
	}
	if len(hist.List()) != 1 {
		t.Fatal("captures should survive proxy stop")
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("second stop: %v", err)
	}

	// Redémarrage avec le même gestionnaire.
	m.commandBuilder = func(_ *Manager, _ string, _ string, _ *Bridge, _ Config) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=TestMintHelperProcess", "--")
		cmd.Env = append(os.Environ(), "GO_WANT_MINT_HELPER=1", "MINT_HELPER_MODE=ready", "MINT_HELPER_MARKER="+marker)
		return cmd, nil
	}
	if _, err := m.Start(context.Background()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if m.Status().State != StateRunning {
		t.Fatalf("restart state = %s", m.Status().State)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(context.Background()); err == nil {
		t.Fatal("start after close should fail")
	}
}

func TestManagerCrashAfterReady(t *testing.T) {
	cfg := testConfig(t)
	m, _ := testManager(t, cfg, "crash-after-ready", filepath.Join(t.TempDir(), "m"))
	notifs := make(chan Notification, 8)
	m.SetNotifier(func(n Notification) { notifs <- n })
	if _, err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return m.Status().State == StateFailed })
	if m.Status().BridgeConnected {
		t.Fatal("bridge should be disconnected after crash")
	}
	select {
	case n := <-notifs:
		if n.Kind != "proxy" || !strings.Contains(n.Message, "inattendue") {
			t.Fatalf("notification = %#v", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing crash notification")
	}
	// Le shell doit pouvoir redémarrer après un échec.
	m.commandBuilder = func(_ *Manager, _ string, _ string, _ *Bridge, _ Config) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=TestMintHelperProcess", "--")
		cmd.Env = append(os.Environ(), "GO_WANT_MINT_HELPER=1", "MINT_HELPER_MODE=ready", "MINT_HELPER_MARKER="+filepath.Join(t.TempDir(), "m"))
		return cmd, nil
	}
	if _, err := m.Start(context.Background()); err != nil {
		t.Fatalf("restart after crash: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerStartTimeout(t *testing.T) {
	cfg := testConfig(t)
	cfg.StartTimeout = 400 * time.Millisecond
	m, _ := testManager(t, cfg, "no-ready", filepath.Join(t.TempDir(), "m"))
	start := time.Now()
	_, err := m.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "confirmé") {
		t.Fatalf("expected timeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took too long: %s", elapsed)
	}
	if m.Status().State != StateFailed {
		t.Fatalf("state = %s", m.Status().State)
	}
}

func TestManagerSetPortWhileRunning(t *testing.T) {
	cfg := testConfig(t)
	m, _ := testManager(t, cfg, "ready", filepath.Join(t.TempDir(), "m"))
	newPort := freePort(t)
	if err := m.SetPort(newPort); err != nil {
		t.Fatalf("set port while stopped: %v", err)
	}
	if _, err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.SetPort(freePort(t)); err == nil || !strings.Contains(err.Error(), "arrêtez") {
		t.Fatalf("expected refusal while running, got %v", err)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.SetPort(freePort(t)); err != nil {
		t.Fatalf("set port after stop: %v", err)
	}
}

func TestManagerDoesNotKillOtherProcesses(t *testing.T) {
	other := exec.Command("sleep", "5")
	if err := other.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	defer func() {
		_ = other.Process.Kill()
		_, _ = other.Process.Wait()
	}()

	cfg := testConfig(t)
	m, _ := testManager(t, cfg, "ready", filepath.Join(t.TempDir(), "m"))
	if _, err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := other.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated process was killed: %v", err)
	}
}

func TestManagerHistoryLimitsApplied(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxCaptures = 5
	cfg.MaxBodyBytes = 1024
	cfg.MaxTotalBodyBytes = 4096
	m, hist := testManager(t, cfg, "ready", filepath.Join(t.TempDir(), "m"))
	if hist.Config().MaxCaptures != 5 {
		t.Fatalf("history config not applied: %#v", hist.Config())
	}
	_ = m
}
