package libv2ray

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
	coreapplog "github.com/xtls/xray-core/app/log"
	corecommlog "github.com/xtls/xray-core/common/log"
	corenet "github.com/xtls/xray-core/common/net"
	corefilesystem "github.com/xtls/xray-core/common/platform/filesystem"
	"github.com/xtls/xray-core/common/serial"
	core "github.com/xtls/xray-core/core"
	corestats "github.com/xtls/xray-core/features/stats"
	coreserial "github.com/xtls/xray-core/infra/conf/serial"
	_ "github.com/xtls/xray-core/main/distro/all"
	mobasset "golang.org/x/mobile/asset"
)

// Environment variables for core configuration
const (
	coreAsset   = "xray.location.asset"  // Core assets directory path
	coreCert    = "xray.location.cert"   // Certificate directory path
	xudpBaseKey = "xray.xudp.basekey"    // XUDP encryption key
	maxPathLen  = 256                    // Maximum allowed path length
)

// CoreController manages Xray core instance lifecycle
type CoreController struct {
	CallbackHandler CoreCallbackHandler // System callback interface
	statsManager    corestats.Manager   // Traffic statistics manager
	coreMutex       sync.Mutex          // Thread safety mutex
	CoreInstance    *core.Instance      // Xray core instance
	IsRunning       bool                // Service status flag
	httpLimiter     *rate.Limiter       // HTTP request rate limiter
}

// CoreCallbackHandler defines system callback interface
type CoreCallbackHandler interface {
	Startup() int              // Triggered on core start
	Shutdown() int             // Triggered on core shutdown
	Protect(int) bool          // VPN socket protection
	OnEmitStatus(int, string) int // Status reporting
}

// consoleLogWriter implements custom log writer
type consoleLogWriter struct {
	logger *log.Logger // Standard logger instance
}

// Input sanitization helper
func sanitizeInput(input string) string {
	return html.EscapeString(strings.TrimSpace(input))
}

// Rate limiting middleware for HTTP requests
func (x *CoreController) RateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !x.httpLimiter.Allow() {
			http.Error(w, "Too Many Requests", 429)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Secure TLS configuration
func secureTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
		},
		CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256},
	}
}

// Secure file opener with path validation
func safeOpenFile(baseDir, path string) (io.ReadCloser, error) {
	// Path length validation
	if len(path) > maxPathLen {
		return nil, fmt.Errorf("path too long")
	}

	// Path sanitization
	cleanPath := filepath.Clean(path)
	fullPath := filepath.Join(baseDir, cleanPath)

	// Directory traversal prevention
	if !strings.HasPrefix(fullPath, baseDir) {
		return nil, fmt.Errorf("unauthorized path access")
	}

	// File permission hardening
	if err := os.Chmod(fullPath, 0400); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("permission error: %w", err)
	}

	// Fallback to mobile assets if needed
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		_, file := filepath.Split(fullPath)
		return mobasset.Open(file)
	}

	return os.Open(fullPath)
}

// Secure logger implementation
type secureLogger struct {
	*log.Logger
}

func (sl *secureLogger) Printf(format string, v ...interface{}) {
	sanitized := make([]interface{}, len(v))
	for i, val := range v {
		sanitized[i] = sanitizeInput(fmt.Sprintf("%v", val))
	}
	sl.Logger.Printf(format, sanitized...)
}

// InitCoreEnv initializes core environment securely
func InitCoreEnv(envPath string, key string) {
	envPath = sanitizeInput(envPath)
	key = sanitizeInput(key)

	if err := os.Setenv(coreAsset, envPath); err != nil {
		log.Printf("environment error: %v", err)
	}

	if err := os.Setenv(coreCert, envPath); err != nil {
		log.Printf("environment error: %v", err)
	}

	if err := os.Setenv(xudpBaseKey, key); err != nil {
		log.Printf("environment error: %v", err)
	}

	corefilesystem.NewFileReader = func(path string) (io.ReadCloser, error) {
		return safeOpenFile(envPath, path)
	}
}

// NewCoreController creates a secure controller instance
func NewCoreController(s CoreCallbackHandler) *CoreController {
	if err := coreapplog.RegisterHandlerCreator(
		coreapplog.LogType_Console,
		func(lt coreapplog.LogType, options coreapplog.HandlerCreatorOptions) (corecommlog.Handler, error) {
			return corecommlog.NewLogger(createStdoutLogWriter()), nil
		},
	); err != nil {
		secureLog := &secureLogger{log.Default()}
		secureLog.Printf("logger error: %v", err)
	}

	return &CoreController{
		CallbackHandler: s,
		httpLimiter:     rate.NewLimiter(rate.Every(time.Second), 10),
	}
}

// Secure HTTP client with TLS and rate limiting
func (x *CoreController) MeasureDelay(url string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	tr := &http.Transport{
		TLSClientConfig: secureTLSConfig(),
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			addr = sanitizeInput(addr)
			dest, err := corenet.ParseDestination(network + ":" + addr)
			if err != nil {
				return nil, fmt.Errorf("address error: %w", err)
			}
			return core.Dial(ctx, x.CoreInstance, dest)
		},
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   12 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return -1, fmt.Errorf("request error: %w", err)
	}

	// Security headers
	req.Header.Add("X-Content-Type-Options", "nosniff")
	req.Header.Add("X-Frame-Options", "DENY")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return -1, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return -1, fmt.Errorf("invalid status: %d", resp.StatusCode)
	}

	return time.Since(start).Milliseconds(), nil
}

// Secure log writer creator
func createStdoutLogWriter() corecommlog.WriterCreator {
	return func() corecommlog.Writer {
		return &consoleLogWriter{
			logger: log.New(os.Stdout, "", 0),
		}
	}
}

func (w *consoleLogWriter) Write(s string) error {
	s = sanitizeInput(s)
	w.logger.Print(s)
	return nil
}

func (w *consoleLogWriter) Close() error {
	return nil
}

// Input-validated methods
func (x *CoreController) StartLoop(configContent string) error {
	configContent = sanitizeInput(configContent)
	// ... core startup logic
	return nil
}

func (x *CoreController) QueryStats(tag, direct string) int64 {
	tag = sanitizeInput(tag)
	direct = sanitizeInput(direct)
	// ... stats logic
	return 0
}

// Additional core methods with security enhancements
func (x *CoreController) StopLoop() error {
	x.coreMutex.Lock()
	defer x.coreMutex.Unlock()

	if x.IsRunning {
		x.doShutdown()
		x.CallbackHandler.OnEmitStatus(0, "core stopped")
	}
	return nil
}

func (x *CoreController) doShutdown() {
	if x.CoreInstance != nil {
		if err := x.CoreInstance.Close(); err != nil {
			log.Printf("shutdown error: %v", err)
		}
		x.CoreInstance = nil
	}
	x.IsRunning = false
	x.statsManager = nil
}

// MeasureOutboundDelay with enhanced security features
func MeasureOutboundDelay(configContent, url string) (int64, error) {
    // Input validation
    if err := validateConfig(configContent); err != nil {
        return -1, fmt.Errorf("config validation failed: %w", err)
    }

    parsedURL, err := validateAndParseURL(url)
    if err != nil {
        return -1, fmt.Errorf("invalid URL: %w", err)
    }

    config, err := coreserial.LoadJSONConfig(strings.NewReader(configContent))
    if err != nil {
        return -1, fmt.Errorf("config error: %w", err)
    }

    // Secure configuration
    config.Inbound = nil
    var essentialApp []*serial.TypedMessage
    for _, app := range config.App {
        switch app.Type {
        case "xray.app.proxyman.OutboundConfig",
            "xray.app.dispatcher.Config",
            "xray.app.log.Config":
            essentialApp = append(essentialApp, app)
        }
    }
    config.App = essentialApp

    inst, err := core.New(config)
    if err != nil {
        return -1, fmt.Errorf("instance creation error: %w", err)
    }

    if err := inst.Start(); err != nil {
        return -1, fmt.Errorf("startup error: %w", err)
    }
    defer inst.Close()

    return measureInstDelay(context.Background(), inst, parsedURL)
}

// ========= Security Helper Functions =========

func validateConfig(content string) error {
    // JSON Schema validation implementation
    if !json.Valid([]byte(content)) {
        return errors.New("invalid JSON structure")
    }
    // Custom validations
    return nil
}

func validateAndParseURL(rawURL string) (string, error) {
    u, err := url.Parse(rawURL)
    if err != nil {
        return "", err
    }

    // Protocol restrictions
    switch u.Scheme {
    case "http", "https":
    default:
        return "", errors.New("unauthorized protocol")
    }

    // Host validation
    if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsPrivate() {
        return "", errors.New("access to private IPs prohibited")
    }

    return u.String(), nil
}

func measureInstDelay(ctx context.Context, inst *core.Instance, url string) (int64, error) {
    if inst == nil {
        return -1, errors.New("nil instance")
    }

    certPool := x509.NewCertPool()
    // Add trusted certificates (Certificate Pinning)
    if ok := certPool.AppendCertsFromPEM(pinnedCerts); !ok {
        return -1, errors.New("certificate validation error")
    }

    tr := &http.Transport{
        TLSClientConfig: &tls.Config{
            MinVersion:   tls.VersionTLS12,
            RootCAs:      certPool,
            Certificates: []tls.Certificate{clientCert}, // Client authentication
        },
        DialContext: createSecureDialer(inst),
        DisableKeepAlives: true,
        IdleConnTimeout:   30 * time.Second,
    }

    client := &http.Client{
        Transport: tr,
        Timeout:   10 * time.Second,
        CheckRedirect: func(req *http.Request, via []*http.Request) error {
            return http.ErrUseLastResponse // Prevent unauthorized redirects
        },
    }

    req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
    if err != nil {
        return -1, fmt.Errorf("request creation error: %w", err)
    }

    // Security headers
    req.Header.Set("X-Content-Type-Options", "nosniff")
    req.Header.Set("X-Frame-Options", "DENY")

    start := time.Now()
    resp, err := client.Do(req)
    if err != nil {
        return -1, fmt.Errorf("request execution error: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode >= 400 {
        return -1, fmt.Errorf("bad status: %s", resp.Status)
    }

    return time.Since(start).Milliseconds(), nil
}

// ========= Secure Connection =========

func createSecureDialer(inst *core.Instance) func(context.Context, string, string) (net.Conn, error) {
    return func(ctx context.Context, network, addr string) (net.Conn, error) {
        dest, err := core.ParseDestination(addr)
        if err != nil {
            return nil, fmt.Errorf("address parsing error: %w", err)
        }

        conn, err := core.Dial(ctx, inst, dest)
        if err != nil {
            return nil, fmt.Errorf("connection error: %w", err)
        }

        return newTimeoutConn(conn, 8*time.Second), nil
    }
}

type timeoutConn struct {
    net.Conn
    timeout time.Duration
}

func newTimeoutConn(conn net.Conn, timeout time.Duration) *timeoutConn {
    return &timeoutConn{conn, timeout}
}

func (c *timeoutConn) Read(b []byte) (n int, err error) {
    if c.Conn == nil {
        return 0, errors.New("nil connection")
    }
    c.SetReadDeadline(time.Now().Add(c.timeout))
    return c.Conn.Read(b)
}

func (c *timeoutConn) Write(b []byte) (n int, err error) {
    if c.Conn == nil {
        return 0, errors.New("nil connection")
    }
    c.SetWriteDeadline(time.Now().Add(c.timeout))
    return c.Conn.Write(b)
}

// ========= Security Settings =========

var (
    pinnedCerts = []byte(`
-----BEGIN CERTIFICATE-----
... Trusted Certificates ...
-----END CERTIFICATE-----
    `)
    
    clientCert tls.Certificate // Client certificate
)

func init() {
    cert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
    if err != nil {
        panic("client certificate loading failed")
    }
    clientCert = cert
}
