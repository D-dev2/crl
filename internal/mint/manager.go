package mint

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"crl-cli/internal/display"
)

//go:embed addon.py
var addonSource []byte

const (
	defaultStartTimeout = 12 * time.Second
	defaultStopTimeout  = 6 * time.Second
	defaultStopGrace    = 4 * time.Second
)

// State représente l'état du processus mitmproxy géré par Mint.
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateFailed   State = "failed"
)

// Config est la configuration effective de Mint.
type Config struct {
	// ListenHost est l'adresse d'écoute du proxy. Seules les adresses
	// loopback sont acceptées.
	ListenHost string
	Port       int

	MaxBodyBytes      int64
	MaxCaptures       int
	MaxTotalBodyBytes int64

	// MitmdumpPath force le chemin de mitmdump. Vide = recherche dans PATH.
	MitmdumpPath string

	StartTimeout time.Duration
	StopTimeout  time.Duration
	StopGrace    time.Duration
}

// DefaultConfig retourne la configuration par défaut, sûre et locale.
func DefaultConfig() Config {
	return Config{
		ListenHost:        "127.0.0.1",
		Port:              8080,
		MaxBodyBytes:      DefaultMaxBodyBytes,
		MaxCaptures:       DefaultMaxCaptures,
		MaxTotalBodyBytes: DefaultMaxTotalBodyBytes,
		StartTimeout:      defaultStartTimeout,
		StopTimeout:       defaultStopTimeout,
		StopGrace:         defaultStopGrace,
	}
}

func (c Config) normalized() Config {
	if c.ListenHost == "" {
		c.ListenHost = "127.0.0.1"
	}
	if c.Port == 0 {
		c.Port = 8080
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if c.MaxCaptures <= 0 {
		c.MaxCaptures = DefaultMaxCaptures
	}
	if c.MaxTotalBodyBytes <= 0 {
		c.MaxTotalBodyBytes = DefaultMaxTotalBodyBytes
	}
	if c.StartTimeout <= 0 {
		c.StartTimeout = defaultStartTimeout
	}
	if c.StopTimeout <= 0 {
		c.StopTimeout = defaultStopTimeout
	}
	if c.StopGrace <= 0 {
		c.StopGrace = defaultStopGrace
	}
	return c
}

// ValidatePort vérifie qu'un numéro de port est utilisable.
func ValidatePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port invalide %d : valeur attendue entre 1 et 65535", port)
	}
	return nil
}

// Validate vérifie la configuration de Mint.
func (c Config) Validate() error {
	if err := ValidatePort(c.Port); err != nil {
		return err
	}
	host := c.ListenHost
	if host == "" {
		host = "127.0.0.1"
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("adresse d'écoute %q refusée : Mint écoute uniquement sur loopback", host)
		}
	}
	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("limite de corps invalide")
	}
	if c.MaxCaptures <= 0 {
		return fmt.Errorf("limite de captures invalide")
	}
	if c.MaxTotalBodyBytes <= 0 {
		return fmt.Errorf("limite mémoire invalide")
	}
	return nil
}

// CheckPortAvailable vérifie qu'un port loopback peut être écouté.
func CheckPortAvailable(host string, port int) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("port %s indisponible : %w", addr, err)
	}
	return ln.Close()
}

// Status est un instantané de l'état du gestionnaire.
type Status struct {
	State           State
	ListenHost      string
	Port            int
	Captures        int
	Responses       int
	BridgeConnected bool
	MitmdumpPath    string
	AddonPath       string
	LastError       string
	StartedAt       time.Time
	TotalBodyBytes  int64
}

// StartInfo décrit un proxy démarré avec succès.
type StartInfo struct {
	ProxyURL     string
	Listen       string
	MitmdumpPath string
	AddonPath    string
}

type session struct {
	bridge     *Bridge
	cmd        *exec.Cmd
	doneCh     chan struct{}
	readyCh    chan struct{}
	addonDir   string
	addonPath  string
	mitmdump   string
	consumerWG sync.WaitGroup
	readyOnce  sync.Once
	closeOnce  sync.Once
	stopping   atomic.Bool
	waitErr    error
}

func (s *session) closeResources() {
	s.closeOnce.Do(func() {
		_ = s.bridge.Close()
		s.consumerWG.Wait()
		if s.addonDir != "" {
			_ = os.RemoveAll(s.addonDir)
		}
	})
}

// Manager pilote le processus mitmdump et relie ses événements à l'historique.
type Manager struct {
	mu   sync.Mutex
	opMu sync.Mutex

	cfg     Config
	hist    *History
	state   State
	lastErr string

	session      *session
	bridgeReady  bool
	startedAt    time.Time
	mitmdumpPath string
	addonPath    string
	closed       bool
	notifier     func(Notification)

	// commandBuilder permet aux tests d'injecter un faux processus.
	commandBuilder func(m *Manager, mitmdumpPath, addonPath string, b *Bridge, cfg Config) (*exec.Cmd, error)
}

// NewManager crée un gestionnaire Mint.
func NewManager(cfg Config, hist *History) (*Manager, error) {
	cfg = cfg.normalized()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if hist == nil {
		hist = NewHistory(HistoryConfig{
			MaxCaptures:       cfg.MaxCaptures,
			MaxBodyBytes:      cfg.MaxBodyBytes,
			MaxTotalBodyBytes: cfg.MaxTotalBodyBytes,
		})
	}
	return &Manager{
		cfg:   cfg,
		hist:  hist,
		state: StateStopped,
	}, nil
}

// History retourne l'historique associé.
func (m *Manager) History() *History { return m.hist }

// SetNotifier installe la fonction d'affichage asynchrone. Elle est appelée
// depuis la goroutine du pont : elle doit être rapide et non bloquante.
func (m *Manager) SetNotifier(fn func(Notification)) {
	m.mu.Lock()
	m.notifier = fn
	m.mu.Unlock()
}

func (m *Manager) notify(n Notification) {
	m.mu.Lock()
	fn := m.notifier
	m.mu.Unlock()
	if fn != nil {
		fn(n)
	}
}

// Config retourne la configuration effective.
func (m *Manager) Config() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// SetPort change le port d'écoute. Refusé si le proxy n'est pas arrêté.
func (m *Manager) SetPort(port int) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	if err := ValidatePort(port); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.state {
	case StateStarting, StateRunning, StateStopping:
		return fmt.Errorf("le proxy est %s : arrêtez-le avec mint.proxy off avant de changer le port", stateLabel(m.state))
	}
	if err := CheckPortAvailable(m.cfg.ListenHost, port); err != nil {
		return fmt.Errorf("%w ; choisissez un autre port avec mint.proxy.port <port>", err)
	}
	m.cfg.Port = port
	return nil
}

// Status retourne un instantané cohérent de l'état.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	status := Status{
		State:           m.state,
		ListenHost:      m.cfg.ListenHost,
		Port:            m.cfg.Port,
		BridgeConnected: m.bridgeReady && m.state == StateRunning,
		MitmdumpPath:    m.mitmdumpPath,
		AddonPath:       m.addonPath,
		LastError:       m.lastErr,
		StartedAt:       m.startedAt,
	}
	if m.hist != nil {
		status.Captures, status.Responses = m.hist.Counts()
		status.TotalBodyBytes = m.hist.TotalBodyBytes()
	}
	return status
}

// ProxyURL retourne l'URL du proxy actif, pour curl.
func (m *Manager) ProxyURL() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != StateRunning {
		return "", false
	}
	return "http://" + net.JoinHostPort(m.cfg.ListenHost, strconv.Itoa(m.cfg.Port)), true
}

// ResolveMitmdump retourne le chemin de mitmdump sans démarrer le proxy.
func (m *Manager) ResolveMitmdump() (string, error) {
	m.mu.Lock()
	configured := m.cfg.MitmdumpPath
	m.mu.Unlock()
	if configured != "" {
		path, err := exec.LookPath(configured)
		if err != nil {
			return "", fmt.Errorf("mitmdump introuvable (%q) : %w", configured, err)
		}
		return path, nil
	}
	path, err := exec.LookPath("mitmdump")
	if err != nil {
		return "", fmt.Errorf("mitmdump introuvable dans PATH. Installez mitmproxy (par exemple : pipx install mitmproxy ou pip install --user mitmproxy), puis relancez CRL-CLI")
	}
	return path, nil
}

// Start démarre mitmdump et attend que l'addon confirme que le proxy est prêt.
func (m *Manager) Start(ctx context.Context) (StartInfo, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return StartInfo{}, fmt.Errorf("gestionnaire Mint fermé")
	}
	if m.state == StateStarting || m.state == StateRunning || m.state == StateStopping {
		state := m.state
		m.mu.Unlock()
		return StartInfo{}, fmt.Errorf("le proxy Mint est déjà %s", stateLabel(state))
	}
	oldSession := m.session
	m.session = nil
	m.state = StateStarting
	m.lastErr = ""
	m.bridgeReady = false
	cfg := m.cfg
	m.mu.Unlock()

	if oldSession != nil {
		oldSession.closeResources()
	}

	fail := func(err error) (StartInfo, error) {
		m.mu.Lock()
		m.state = StateFailed
		m.lastErr = err.Error()
		m.bridgeReady = false
		m.session = nil
		m.mitmdumpPath = ""
		m.addonPath = ""
		m.mu.Unlock()
		return StartInfo{}, err
	}

	if err := cfg.Validate(); err != nil {
		return fail(err)
	}
	mitmdumpPath, err := m.ResolveMitmdump()
	if err != nil {
		return fail(err)
	}
	if err := CheckPortAvailable(cfg.ListenHost, cfg.Port); err != nil {
		return fail(fmt.Errorf("%w ; choisissez un autre port avec mint.proxy.port <port>", err))
	}

	bridge, err := NewBridge(cfg.MaxBodyBytes)
	if err != nil {
		return fail(err)
	}

	dir, err := os.MkdirTemp("", "crl-mint-*")
	if err != nil {
		_ = bridge.Close()
		return fail(fmt.Errorf("création du répertoire de l'addon : %w", err))
	}
	_ = os.Chmod(dir, 0o700)
	addonPath := filepath.Join(dir, "addon.py")
	if err := os.WriteFile(addonPath, addonSource, 0o600); err != nil {
		_ = bridge.Close()
		_ = os.RemoveAll(dir)
		return fail(fmt.Errorf("écriture de l'addon Mint : %w", err))
	}

	cmd, err := m.buildCommand(mitmdumpPath, addonPath, bridge, cfg)
	if err != nil {
		_ = bridge.Close()
		_ = os.RemoveAll(dir)
		return fail(err)
	}
	baseEnv := cmd.Env
	if baseEnv == nil {
		baseEnv = os.Environ()
	}
	cmd.Env = append(baseEnv,
		"MINT_BRIDGE_HOST="+bridge.Host(),
		"MINT_BRIDGE_PORT="+strconv.Itoa(bridge.Port()),
		"MINT_BRIDGE_TOKEN="+bridge.token,
		"MINT_MAX_BODY_BYTES="+strconv.FormatInt(cfg.MaxBodyBytes, 10),
		"MINT_BRIDGE_TIMEOUT_MS=2000",
		"MINT_EVENT_QUEUE=4096",
	)
	configureProcessGroup(cmd)
	stderr := newTailBuffer(16 << 10)
	if cmd.Stderr == nil {
		cmd.Stderr = stderr
	}
	if cmd.Stdout == nil {
		cmd.Stdout = io.Discard
	}

	if err := cmd.Start(); err != nil {
		_ = bridge.Close()
		_ = os.RemoveAll(dir)
		return fail(fmt.Errorf("démarrage de mitmdump : %w", err))
	}

	s := &session{
		bridge:    bridge,
		cmd:       cmd,
		doneCh:    make(chan struct{}),
		readyCh:   make(chan struct{}),
		addonDir:  dir,
		addonPath: addonPath,
		mitmdump:  mitmdumpPath,
	}
	m.mu.Lock()
	m.session = s
	m.mitmdumpPath = mitmdumpPath
	m.addonPath = addonPath
	m.mu.Unlock()

	s.consumerWG.Add(1)
	go m.consume(s)
	go m.monitor(s, stderr)

	timer := time.NewTimer(cfg.StartTimeout)
	defer timer.Stop()

	select {
	case <-s.readyCh:
		m.mu.Lock()
		if m.session == s && m.state == StateStarting {
			m.state = StateRunning
			m.startedAt = time.Now()
			m.bridgeReady = true
			m.lastErr = ""
		}
		m.mu.Unlock()
		return StartInfo{
			ProxyURL:     "http://" + net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.Port)),
			Listen:       net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.Port)),
			MitmdumpPath: mitmdumpPath,
			AddonPath:    addonPath,
		}, nil
	case <-s.doneCh:
		detail := m.processFailure(s, stderr)
		return fail(detail)
	case <-timer.C:
		m.stopSession(s, cfg.StopGrace)
		return fail(fmt.Errorf("mitmproxy n'a pas confirmé son démarrage dans les %s ; le processus a été arrêté", cfg.StartTimeout))
	case <-ctx.Done():
		m.stopSession(s, cfg.StopGrace)
		return fail(fmt.Errorf("démarrage interrompu : %w", ctx.Err()))
	}
}

func (m *Manager) buildCommand(mitmdumpPath, addonPath string, bridge *Bridge, cfg Config) (*exec.Cmd, error) {
	if m.commandBuilder != nil {
		return m.commandBuilder(m, mitmdumpPath, addonPath, bridge, cfg)
	}
	args := []string{
		"--quiet",
		"--listen-host", cfg.ListenHost,
		"--listen-port", strconv.Itoa(cfg.Port),
		"--set", "flow_detail=0",
		"--set", "console_eventlog_verbosity=error",
		"-s", addonPath,
	}
	return exec.Command(mitmdumpPath, args...), nil
}

func (m *Manager) consume(s *session) {
	defer s.consumerWG.Done()
	for ev := range s.bridge.Events() {
		m.applyEvent(s, ev)
	}
}

func (m *Manager) applyEvent(s *session, ev BridgeEvent) {
	switch ev.Type {
	case "ready":
		m.mu.Lock()
		if m.session == s {
			m.bridgeReady = true
		}
		m.mu.Unlock()
		s.readyOnce.Do(func() { close(s.readyCh) })
	case "request":
		capture, _ := m.hist.AddRequest(ev)
		m.notify(Notification{
			Kind:   "request",
			Level:  "info",
			ID:     capture.ID,
			Method: capture.Request.Method,
			URL:    display.MaskURL(capture.Request.URL),
		})
	case "response":
		capture := m.hist.AddResponse(ev)
		m.notify(Notification{
			Kind:     "response",
			Level:    "info",
			ID:       capture.ID,
			Status:   ev.StatusCode,
			Duration: capture.Duration,
		})
	case "error":
		capture := m.hist.AddError(ev.FlowID, ev.Error, ev.Timestamp)
		m.notify(Notification{
			Kind:    "error",
			Level:   "warning",
			ID:      capture.ID,
			Message: ev.Error,
		})
	}
}

func (m *Manager) monitor(s *session, stderr *tailBuffer) {
	s.waitErr = s.cmd.Wait()
	unexpected := !s.stopping.Load()

	m.mu.Lock()
	if m.session == s {
		m.bridgeReady = false
		if unexpected {
			m.state = StateFailed
			m.lastErr = m.processFailure(s, stderr).Error()
			m.session = nil
			m.mitmdumpPath = ""
			m.addonPath = ""
		}
	}
	m.mu.Unlock()

	close(s.doneCh)

	if unexpected {
		s.closeResources()
		message := m.processFailure(s, stderr)
		m.notify(Notification{
			Kind:    "proxy",
			Level:   "error",
			Message: "mitmdump s'est arrêté de manière inattendue : " + message.Error(),
		})
	}
}

func (m *Manager) processFailure(s *session, stderr *tailBuffer) error {
	var message string
	if s.waitErr != nil {
		message = s.waitErr.Error()
	} else {
		message = "processus terminé sans erreur explicite"
	}
	tail := strings.TrimSpace(stderr.String())
	if tail != "" {
		tail = strings.ReplaceAll(tail, s.bridge.token, "***")
		if len(tail) > 3000 {
			tail = tail[len(tail)-3000:]
		}
		message += " — " + tail
	}
	return fmt.Errorf("mitmdump : %s", message)
}

// Stop arrête uniquement le processus lancé par ce gestionnaire.
func (m *Manager) Stop(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	s := m.session
	if s == nil {
		if !m.closed {
			m.state = StateStopped
			m.bridgeReady = false
			m.lastErr = ""
			m.startedAt = time.Time{}
			m.mitmdumpPath = ""
			m.addonPath = ""
		}
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	grace := m.Config().StopGrace
	select {
	case <-ctx.Done():
		m.stopSession(s, 0)
	default:
		m.stopSession(s, grace)
	}
	return nil
}

func (m *Manager) stopSession(s *session, grace time.Duration) {
	s.stopping.Store(true)
	m.mu.Lock()
	if m.session == s {
		m.state = StateStopping
	}
	m.mu.Unlock()

	if grace > 0 {
		_ = signalProcessGroup(s.cmd, interruptSignal())
		timer := time.NewTimer(grace)
		select {
		case <-s.doneCh:
			timer.Stop()
		case <-timer.C:
			_ = killProcessGroup(s.cmd)
			select {
			case <-s.doneCh:
			case <-time.After(2 * time.Second):
			}
		}
	} else {
		_ = killProcessGroup(s.cmd)
		select {
		case <-s.doneCh:
		case <-time.After(2 * time.Second):
		}
	}

	s.closeResources()

	m.mu.Lock()
	if m.session == s {
		m.session = nil
		m.state = StateStopped
		m.bridgeReady = false
		m.startedAt = time.Time{}
		m.mitmdumpPath = ""
		m.addonPath = ""
	}
	m.mu.Unlock()
}

// Close arrête le proxy et interdit tout démarrage ultérieur.
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), m.Config().StopTimeout)
	defer cancel()
	return m.Stop(ctx)
}

func stateLabel(state State) string {
	switch state {
	case StateStopped:
		return "arrêté"
	case StateStarting:
		return "en démarrage"
	case StateRunning:
		return "actif"
	case StateStopping:
		return "en arrêt"
	case StateFailed:
		return "en erreur"
	default:
		return string(state)
	}
}

type tailBuffer struct {
	mu   sync.Mutex
	data []byte
	max  int
}

func newTailBuffer(max int) *tailBuffer {
	if max <= 0 {
		max = 4096
	}
	return &tailBuffer{max: max}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if len(t.data) > t.max {
		t.data = append([]byte(nil), t.data[len(t.data)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.data)
}
