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

type CoreController struct {
	CallbackHandler CoreCallbackHandler
	statsManager    corestats.Manager
	coreMutex       sync.Mutex
	CoreInstance    *core.Instance
	IsRunning       bool
}

type CoreCallbackHandler interface {
	Startup() int
	Shutdown() int
	Protect(int) bool
	OnEmitStatus(int, string) int
}

type consoleLogWriter struct {
	logger *log.Logger
}

func InitCoreEnv(envPath string, key string) {
	if len(envPath) > 0 {
		if err := os.Setenv(coreAsset, envPath); err != nil { // Line 63: Error handling added
			log.Printf("failed to set %s: %v", coreAsset, err)
		}
		if err := os.Setenv(coreCert, envPath); err != nil { // Line 64: Error handling added
			log.Printf("failed to set %s: %v", coreCert, err)
		}
	}
	if len(key) > 0 {
		if err := os.Setenv(xudpBaseKey, key); err != nil { // Line 67: Error handling added
			log.Printf("failed to set %s: %v", xudpBaseKey, err)
		}
	}

	corefilesystem.NewFileReader = func(path string) (io.ReadCloser, error) {
		// G304 Fix: Path validation
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
		return os.Open(fullPath) // Line 76: Validated path
	}
}

func NewCoreController(s CoreCallbackHandler) *CoreController {
	if err := coreapplog.RegisterHandlerCreator( // Lines 83-87: Error handling added
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

func (x *CoreController) StartLoop(configContent string) (err error) {
	x.coreMutex.Lock()
	defer x.coreMutex.Unlock()

	if x.IsRunning {
		log.Println("The instance is already running")
		return nil
	}

	err = x.doStartLoop(configContent)
	return
}

func (x *CoreController) StopLoop() error {
	x.coreMutex.Lock()
	defer x.coreMutex.Unlock()

	if x.IsRunning {
		x.doShutdown()
		log.Println("Shut down the running instance")
		x.CallbackHandler.OnEmitStatus(0, "Closed")
	}
	return nil
}

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

func (x *CoreController) MeasureDelay(url string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	return measureInstDelay(ctx, x.CoreInstance, url)
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

	if err := inst.Start(); err != nil { // Line 169: Error handling added
		return -1, fmt.Errorf("failed to start core instance: %w", err)
	}
	defer func() {
		if err := inst.Close(); err != nil { // Line 170: Error handling added
			log.Printf("failed to close instance: %v", err)
		}
	}()
	return measureInstDelay(context.Background(), inst, url)
}

func (x *CoreController) doShutdown() {
	if x.CoreInstance != nil {
		if err := x.CoreInstance.Close(); err != nil { // Line 183: Error handling added
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

func measureInstDelay(ctx context.Context, inst *core.Instance, url string) (int64, error) {
	if inst == nil {
		return -1, errors.New("core instance is nil")
	}

	tr := &http.Transport{
		TLSHandshakeTimeout: 6*time.Second,
		DisableKeepAlives: true,
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
		Timeout: 12*time.Second,
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

func createStdoutLogWriter() corecommlog.WriterCreator {
	return func() corecommlog.Writer {
		return &consoleLogWriter{
			logger: log.New(os.Stdout, "", 0),
		}
	}
}
