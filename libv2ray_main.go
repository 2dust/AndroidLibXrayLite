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

	v2applog "github.com/xtls/xray-core/app/log"
	v2commlog "github.com/xtls/xray-core/common/log"
	v2net "github.com/xtls/xray-core/common/net"
	v2filesystem "github.com/xtls/xray-core/common/platform/filesystem"
	"github.com/xtls/xray-core/common/serial"
	v2core "github.com/xtls/xray-core/core"
	v2stats "github.com/xtls/xray-core/features/stats"
	v2serial "github.com/xtls/xray-core/infra/conf/serial"
	_ "github.com/xtls/xray-core/main/distro/all"
	mobasset "golang.org/x/mobile/asset"
)

const (
	v2Asset     = "xray.location.asset"
	v2Cert      = "xray.location.cert"
	xudpBaseKey = "xray.xudp.basekey"
)

// V2RayPoint represents a V2Ray Point Server
type V2RayPoint struct {
	SupportSet   V2RayVPNServiceSupportsSet
	statsManager v2stats.Manager

	v2rayOP sync.Mutex

	Vpoint    *v2core.Instance
	IsRunning bool
}

// V2RayVPNServiceSupportsSet is an interface to support Android VPN mode
type V2RayVPNServiceSupportsSet interface {
	Setup(Conf string) int
	Prepare() int
	Shutdown() int
	Protect(int) bool
	OnEmitStatus(int, string) int
}

// consoleLogWriter creates our own log writer without datetime stamp
// As Android adds time stamps on each line
type consoleLogWriter struct {
	logger *log.Logger
}

// InitV2Env sets the V2Ray asset path
// This function initializes the environment variables for V2Ray,
// including asset path and XUDP base key. It also overrides the default
// file reader to support Android asset system.
func InitV2Env(envPath string, key string) {
	if len(envPath) > 0 {
		os.Setenv(v2Asset, envPath)
		os.Setenv(v2Cert, envPath)
	}
	if len(key) > 0 {
		os.Setenv(xudpBaseKey, key)
	}

	v2filesystem.NewFileReader = func(path string) (io.ReadCloser, error) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			_, file := filepath.Split(path)
			return mobasset.Open(file)
		}
		return os.Open(path)
	}
}

// NewV2RayPoint creates a new V2RayPoint instance
func NewV2RayPoint(s V2RayVPNServiceSupportsSet) *V2RayPoint {
	v2applog.RegisterHandlerCreator(v2applog.LogType_Console,
		func(lt v2applog.LogType,
			options v2applog.HandlerCreatorOptions) (v2commlog.Handler, error) {
			return v2commlog.NewLogger(createStdoutLogWriter()), nil
		})

	return &V2RayPoint{
		SupportSet: s,
	}
}

// RunLoop runs the V2Ray main loop
func (v *V2RayPoint) RunLoop(configContent string) (err error) {
	v.v2rayOP.Lock()
	defer v.v2rayOP.Unlock()

	if v.IsRunning {
		return nil
	}

	err = v.pointloop(configContent)
	return
}

// StopLoop stops the V2Ray main loop
func (v *V2RayPoint) StopLoop() error {
	v.v2rayOP.Lock()
	defer v.v2rayOP.Unlock()

	if v.IsRunning {
		v.shutdownInit()
		v.SupportSet.OnEmitStatus(0, "Closed")
	}
	return nil
}

// QueryStats returns the traffic stats for a given tag and direction
func (v V2RayPoint) QueryStats(tag string, direct string) int64 {
	if v.statsManager == nil {
		return 0
	}
	counter := v.statsManager.GetCounter(fmt.Sprintf("outbound>>>%s>>>traffic>>>%s", tag, direct))
	if counter == nil {
		return 0
	}
	return counter.Set(0)
}

// MeasureDelay measures the delay to a given URL
func (v *V2RayPoint) MeasureDelay(url string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	return measureInstDelay(ctx, v.Vpoint, url)
}

// MeasureOutboundDelay measures the outbound delay for a given configuration and URL
func MeasureOutboundDelay(ConfigureFileContent string, url string) (int64, error) {
	config, err := v2serial.LoadJSONConfig(strings.NewReader(ConfigureFileContent))
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

	inst, err := v2core.New(config)
	if err != nil {
		return -1, fmt.Errorf("failed to create core instance: %w", err)
	}

	inst.Start()
	defer inst.Close()
	return measureInstDelay(context.Background(), inst, url)
}

// CheckVersionX returns the library and V2Ray versions
func CheckVersionX() string {
	var version = 32
	return fmt.Sprintf("Lib v%d, Xray-core v%s", version, v2core.Version())
}

// shutdownInit shuts down the V2Ray instance and cleans up resources
func (v *V2RayPoint) shutdownInit() {
	if v.Vpoint != nil {
		v.Vpoint.Close()
		v.Vpoint = nil
	}
	v.IsRunning = false
	v.statsManager = nil
}

// pointloop sets up and starts the V2Ray core
func (v *V2RayPoint) pointloop(configContent string) error {
	log.Println("Loading core config")
	config, err := v2serial.LoadJSONConfig(strings.NewReader(configContent))
	if err != nil {
		return fmt.Errorf("failed to load core config: %w", err)
	}

	log.Println("Creating new core instance")
	v.Vpoint, err = v2core.New(config)
	if err != nil {
		return fmt.Errorf("failed to create core instance: %w", err)
	}
	v.statsManager = v.Vpoint.GetFeature(v2stats.ManagerType()).(v2stats.Manager)

	log.Println("Starting core")
	v.IsRunning = true
	if err := v.Vpoint.Start(); err != nil {
		v.IsRunning = false
		return fmt.Errorf("failed to start core: %w", err)
	}

	v.SupportSet.Prepare()
	v.SupportSet.Setup("")
	v.SupportSet.OnEmitStatus(0, "Running")
	return nil
}

// measureInstDelay measures the delay for an instance to a given URL
func measureInstDelay(ctx context.Context, inst *v2core.Instance, url string) (int64, error) {
	if inst == nil {
		return -1, errors.New("core instance is nil")
	}

	tr := &http.Transport{
		TLSHandshakeTimeout: 6 * time.Second,
		DisableKeepAlives:   true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dest, err := v2net.ParseDestination(fmt.Sprintf("%s:%s", network, addr))
			if err != nil {
				return nil, err
			}
			return v2core.Dial(ctx, inst, dest)
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
func createStdoutLogWriter() v2commlog.WriterCreator {
	return func() v2commlog.Writer {
		return &consoleLogWriter{
			logger: log.New(os.Stdout, "", 0),
		}
	}
}
