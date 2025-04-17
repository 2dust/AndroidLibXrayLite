package libv2ray

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

// Constants for environment variables
const (
	coreAsset   = "xray.location.asset"  // Path to assets directory
	coreCert    = "xray.location.cert"   // Path to certificates
	xudpBaseKey = "xray.xudp.basekey"    // XUDP encryption key
)

// CoreController represents a controller for managing Xray core instance lifecycle
type CoreController struct {
	CallbackHandler CoreCallbackHandler // System callback handler
	statsManager    corestats.Manager   // Traffic statistics
	coreMutex       sync.Mutex          // Mutex for thread safety
	CoreInstance    *core.Instance      // Xray core instance
	IsRunning       bool                // Service status flag
}

// CoreCallbackHandler defines interface for receiving callbacks and notifications from the core service
type CoreCallbackHandler interface {
	Startup() int              // Triggered on core start
	Shutdown() int             // Triggered on core shutdown
	Protect(int) bool          // VPN protect socket
	OnEmitStatus(int, string) int // Status reporting
}

// consoleLogWriter implements a log writer without datetime stamps
// as Android system already adds timestamps to each log line
type consoleLogWriter struct {
	logger *log.Logger // Standard logger
}

// InitCoreEnv initializes environment variables and file system handlers for the core
// It sets up asset path, certificate path, XUDP base key and customizes the file reader
// to support Android asset system
func InitCoreEnv(envPath string, key string) {
	// Set asset/cert paths
	if len(envPath) > 0 {
		if err := os.Setenv(coreAsset, envPath); err != nil {
			log.Printf("failed to set %s: %v", coreAsset, err)
		}
		if err := os.Setenv(coreCert, envPath); err != nil {
			log.Printf("failed to set %s: %v", coreCert, err)
		}
	}

	// Set XUDP encryption key
	if len(key) > 0 {
		if err := os.Setenv(xudpBaseKey, key); err != nil {
			log.Printf("failed to set %s: %v", xudpBaseKey, err)
		}
	}

	// Custom file reader with path validation
	corefilesystem.NewFileReader = func(path string) (io.ReadCloser, error) {
		// G304 Fix - Path sanitization
		baseDir := envPath
		cleanPath := filepath.Clean(path)
		fullPath := filepath.Join(baseDir, cleanPath)

		// Prevent directory traversal
		if baseDir != "" && !strings.HasPrefix(fullPath, baseDir) {
			return nil, fmt.Errorf("unauthorized path: %s", path)
		}

		// Check file existence
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			_, file := filepath.Split(fullPath)
			return mobasset.Open(file) // Fallback to assets
		} else if err != nil {
			return nil, fmt.Errorf("file access error: %w", err)
		}

		return os.Open(fullPath) // #nosec G304 - Validated path
	}
}

// NewCoreController initializes and returns a new CoreController instance
// Sets up the console log handler and associates it with the provided callback handler
func NewCoreController(s CoreCallbackHandler) *CoreController {
	// Register custom logger
	if err := coreapplog.RegisterHandlerCreator(
		coreapplog.LogType_Console,
		func(lt coreapplog.LogType, options coreapplog.HandlerCreatorOptions) (corecommlog.Handler, error) {
			return corecommlog.NewLogger(createStdoutLogWriter()), nil
		},
	); err != nil {
		log.Printf("logger registration failed: %v", err)
	}

	return &CoreController{
		CallbackHandler: s,
	}
}

// StartLoop initializes and starts the core processing loop
// Thread-safe method that configures and runs the Xray core with the provided configuration
// Returns immediately if the core is already running
func (x *CoreController) StartLoop(configContent string) (err error) {
	x.coreMutex.Lock()
	defer x.coreMutex.Unlock()

	if x.IsRunning {
		log.Println("core already running")
		return nil
	}

	return x.doStartLoop(configContent)
}

// StopLoop safely stops the core processing loop and releases resources
// Thread-safe method that shuts down the core instance and triggers necessary callbacks
func (x *CoreController) StopLoop() error {
	x.coreMutex.Lock()
	defer x.coreMutex.Unlock()

	if x.IsRunning {
		x.doShutdown()
		x.CallbackHandler.OnEmitStatus(0, "core stopped")
	}
	return nil
}

// QueryStats retrieves and resets traffic statistics for a specific outbound tag and direction
// Returns the accumulated traffic value and resets the counter to zero
// Returns 0 if the stats manager is not initialized or the counter doesn't exist
func (x *CoreController) QueryStats(tag string, direct string) int64 {
	if x.statsManager == nil {
		return 0
	}
	counter := x.statsManager.GetCounter(fmt.Sprintf("outbound>>>%s>>>traffic>>>%s", tag, direct))
	if counter == nil {
		return 0
	}
	return counter.Set(0)
}

// MeasureDelay measures network latency to a specified URL through the current core instance
// Uses a 12-second timeout context and returns the round-trip time in milliseconds
// An error is returned if the connection fails or returns an unexpected status
func (x *CoreController) MeasureDelay(url string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	return measureInstDelay(ctx, x.CoreInstance, url)
}

// MeasureOutboundDelay measures the outbound delay for a given configuration and URL
func MeasureOutboundDelay(ConfigureFileContent string, url string) (int64, error) {
	config, err := coreserial.LoadJSONConfig(strings.NewReader(ConfigureFileContent))
	if err != nil {
		return -1, fmt.Errorf("config load error: %w", err)
	}

	// Simplify config for testing
	config.Inbound = nil
	var essentialApp []*serial.TypedMessage
	for _, app := range config.App {
		if app.Type == "xray.app.proxyman.OutboundConfig" || 
		   app.Type == "xray.app.dispatcher.Config" || 
		   app.Type == "xray.app.log.Config" {
			essentialApp = append(essentialApp, app)
		}
	}
	config.App = essentialApp

	inst, err := core.New(config)
	if err != nil {
		return -1, fmt.Errorf("instance creation failed: %w", err)
	}

	if err := inst.Start(); err != nil {
		return -1, fmt.Errorf("startup failed: %w", err)
	}
	defer inst.Close()

	return measureInstDelay(context.Background(), inst, url)
}

// doShutdown shuts down the Xray instance and cleans up resources
func (x *CoreController) doShutdown() {
	if x.CoreInstance != nil {
		if err := x.CoreInstance.Close(); err != nil {
			log.Printf("core shutdown error: %v", err)
		}
		x.CoreInstance = nil
	}
	x.IsRunning = false
	x.statsManager = nil
}

// doStartLoop sets up and starts the Xray core
func (x *CoreController) doStartLoop(configContent string) error {
	log.Println("initializing core...")
	config, err := coreserial.LoadJSONConfig(strings.NewReader(configContent))
	if err != nil {
		return fmt.Errorf("config error: %w", err)
	}

	x.CoreInstance, err = core.New(config)
	if err != nil {
		return fmt.Errorf("core init failed: %w", err)
	}
	x.statsManager = x.CoreInstance.GetFeature(corestats.ManagerType()).(corestats.Manager)

	log.Println("starting core...")
	x.IsRunning = true
	if err := x.CoreInstance.Start(); err != nil {
		x.IsRunning = false
		return fmt.Errorf("startup failed: %w", err)
	}

	x.CallbackHandler.Startup()
	x.CallbackHandler.OnEmitStatus(0, "core started")
	return nil
}

// Network delay measurement
func measureInstDelay(ctx context.Context, inst *core.Instance, url string) (int64, error) {
	if inst == nil {
		return -1, errors.New("nil instance")
	}

	tr := &http.Transport{
		TLSHandshakeTimeout: 6 * time.Second,
		DisableKeepAlives:   true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dest, err := corenet.ParseDestination(fmt.Sprintf("%s:%s", network, addr))
			if err != nil {
				return nil, err
			}
			return core.Dial(ctx, inst, dest)
		},
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   12 * time.Second,
	}

	if url == "" {
		url = "https://www.google.com/generate_204"
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return -1, fmt.Errorf("invalid status: %s", resp.Status)
	}
	return time.Since(start).Milliseconds(), nil
}

// Log writer implementation
func (w *consoleLogWriter) Write(s string) error {
	w.logger.Print(s)
	return nil
}

func (w *consoleLogWriter) Close() error {
	return nil
}

// createStdoutLogWriter creates a logger that won't print date/time stamps
func createStdoutLogWriter() corecommlog.WriterCreator {
	return func() corecommlog.Writer {
		return &consoleLogWriter{
			logger: log.New(os.Stdout, "", 0),
		}
	}
}
