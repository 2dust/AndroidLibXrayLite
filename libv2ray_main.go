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

const (
	coreAsset   = "xray.location.asset"
	coreCert    = "xray.location.cert"
	xudpBaseKey = "xray.xudp.basekey"
)

// CoreController represents a controller for managing Xray core instance lifecycle
func InitCoreEnv(envPath string, key string) {
	if len(envPath) > 0 {
		if err := os.Setenv(coreAsset, envPath); err != nil {
			log.Printf("failed to set %s: %v", coreAsset, err)
		}
		if err := os.Setenv(coreCert, envPath); err != nil {
			log.Printf("failed to set %s: %v", coreCert, err)
		}
	}
	if len(key) > 0 {
		if err := os.Setenv(xudpBaseKey, key); err != nil {
			log.Printf("failed to set %s: %v", xudpBaseKey, err)
		}
	}

	corefilesystem.NewFileReader = func(path string) (io.ReadCloser, error) {
		// G304: Prevent path traversal attacks
		baseDir := envPath
		cleanPath := filepath.Clean(path)
		fullPath := filepath.Join(baseDir, cleanPath)
		if baseDir != "" && !strings.HasPrefix(fullPath, baseDir) {
			return nil, fmt.Errorf("unauthorized file path: %s", path)
		}
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			_, file := filepath.Split(fullPath)
			return mobasset.Open(file)
		} else if err != nil {
			return nil, fmt.Errorf("failed to stat file: %w", err)
		}
		f, err := os.Open(fullPath)
		if err != nil {
			return nil, fmt.Errorf("failed to open file: %w", err)
		}
		return f, nil
	}
}

func NewCoreController(s CoreCallbackHandler) *CoreController {
	if err := coreapplog.RegisterHandlerCreator(
		coreapplog.LogType_Console,
		func(lt coreapplog.LogType, options coreapplog.HandlerCreatorOptions) (corecommlog.Handler, error) {
			return corecommlog.NewLogger(createStdoutLogWriter()), nil
		},
	); err != nil {
		log.Printf("failed to register log handler: %v", err)
	}
	return &CoreController{
		CallbackHandler: s,
	}
}

func (x *CoreController) doShutdown() {
	if x.CoreInstance != nil {
		if err := x.CoreInstance.Close(); err != nil {
			log.Printf("failed to close core instance: %v", err)
		}
		x.CoreInstance = nil
	}
	x.IsRunning = false
	x.statsManager = nil
}

func (x *CoreController) doStartLoop(configContent string) error {
	log.Println("Loading core config")
	config, err := coreserial.LoadJSONConfig(strings.NewReader(configContent))
	if err != nil {
		return fmt.Errorf("failed to load core config: %w", err)
	}

	log.Println("Creating new core instance")
	x.CoreInstance, err = core.New(config)
	if err != nil {
		return fmt.Errorf("failed to create core instance: %w", err)
	}
	x.statsManager = x.CoreInstance.GetFeature(corestats.ManagerType()).(corestats.Manager)

	log.Println("Starting core")
	x.IsRunning = true
	if err := x.CoreInstance.Start(); err != nil {
		x.IsRunning = false
		return fmt.Errorf("failed to start core: %w", err)
	}

	x.CallbackHandler.Startup()
	x.CallbackHandler.OnEmitStatus(0, "Started successfully, running")

	log.Println("Starting core successfully")
	return nil
}

func MeasureOutboundDelay(ConfigureFileContent string, url string) (int64, error) {
	config, err := coreserial.LoadJSONConfig(strings.NewReader(ConfigureFileContent))
	if err != nil {
		return -1, fmt.Errorf("failed to load JSON config: %w", err)
	}

	config.Inbound = nil
	var essentialApp []*serial.TypedMessage
	for _, app := range config.App {
		if app.Type == "xray.app.proxyman.OutboundConfig" || app.Type == "xray.app.dispatcher.Config" || app.Type == "xray.app.log.Config" {
			essentialApp = append(essentialApp, app)
		}
	}
	config.App = essentialApp

	inst, err := core.New(config)
	if err != nil {
		return -1, fmt.Errorf("failed to create core instance: %w", err)
	}

	if err := inst.Start(); err != nil {
		return -1, fmt.Errorf("failed to start core instance: %w", err)
	}
	defer func() {
		if err := inst.Close(); err != nil {
			log.Printf("failed to close instance: %v", err)
		}
	}()
	return measureInstDelay(context.Background(), inst, url)
}
type CoreController struct {
	CallbackHandler CoreCallbackHandler
	statsManager    corestats.Manager
	coreMutex       sync.Mutex
	CoreInstance    *core.Instance
	IsRunning       bool
}

// CoreCallbackHandler defines interface for receiving callbacks and notifications from the core service
type CoreCallbackHandler interface {
	Startup() int
	Shutdown() int
	Protect(int) bool
	OnEmitStatus(int, string) int
}

// consoleLogWriter implements a log writer without datetime stamps
// as Android system already adds timestamps to each log line
type consoleLogWriter struct {
	logger *log.Logger
}

// InitCoreEnv initializes environment variables and file system handlers for the core
// It sets up asset path, certificate path, XUDP base key and customizes the file reader
// to support Android asset system

}

// CheckVersionX returns the library and Xray versions
func CheckVersionX() string {
	var version = 31
	return fmt.Sprintf("Lib v%d, Xray-core v%s", version, core.Version())
}

// doShutdown shuts down the Xray instance and cleans up resources
func (x *CoreController) doShutdown() {
	if x.CoreInstance != nil {
		x.CoreInstance.Close()
		x.CoreInstance = nil
	}
	x.IsRunning = false
	x.statsManager = nil
}

// doStartLoop sets up and starts the Xray core
func (x *CoreController) doStartLoop(configContent string) error {
	log.Println("Loading core config")
	config, err := coreserial.LoadJSONConfig(strings.NewReader(configContent))
	if err != nil {
		return fmt.Errorf("failed to load core config: %w", err)
	}

	log.Println("Creating new core instance")
	x.CoreInstance, err = core.New(config)
	if err != nil {
		return fmt.Errorf("failed to create core instance: %w", err)
	}
	x.statsManager = x.CoreInstance.GetFeature(corestats.ManagerType()).(corestats.Manager)

	log.Println("Starting core")
	x.IsRunning = true
	if err := x.CoreInstance.Start(); err != nil {
		x.IsRunning = false
		return fmt.Errorf("failed to start core: %w", err)
	}

	x.CallbackHandler.Startup()
	x.CallbackHandler.OnEmitStatus(0, "Started successfully, running")

	log.Println("Starting core successfully")
	return nil
}

// measureInstDelay measures the delay for an instance to a given URL
func measureInstDelay(ctx context.Context, inst *core.Instance, url string) (int64, error) {
	if inst == nil {
		return -1, errors.New("core instance is nil")
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

	if len(url) == 0 {
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
		return -1, fmt.Errorf("unexpected status code: %s", resp.Status)
	}
	return time.Since(start).Milliseconds(), nil
}

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
