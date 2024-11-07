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

	mobasset "golang.org/x/mobile/asset"
	v2net "github.com/xtls/xray-core/common/net"
	v2filesystem "github.com/xtls/xray-core/common/platform/filesystem"
	v2core "github.com/xtls/xray-core/core"
	v2stats "github.com/xtls/xray-core/features/stats"
	v2serial "github.com/xtls/xray-core/infra/conf/serial"
	_ "github.com/xtls/xray-core/main/distro/all" // Import all distributions
	v2internet "github.com/xtls/xray-core/transport/internet"
	v2applog "github.com/xtls/xray-core/app/log"
	v2commlog "github.com/xtls/xray-core/common/log"
)

const (
	v2Asset     = "xray.location.asset"
	xudpBaseKey = "xray.xudp.basekey"
)

// V2RayPoint represents a V2Ray Point Server.
type V2RayPoint struct {
	SupportSet           V2RayVPNServiceSupportsSet
	statsManager         v2stats.Manager
	dialer              *ProtectedDialer
	v2rayOP             sync.Mutex
	closeChan           chan struct{}
	Vpoint              *v2core.Instance
	IsRunning           bool
	DomainName          string
	ConfigureFileContent string
	AsyncResolve        bool
}

// V2RayVPNServiceSupportsSet defines methods to support Android VPN mode.
type V2RayVPNServiceSupportsSet interface {
	Setup(Conf string) int
	Prepare() int
	Shutdown() int
	Protect(int) bool
	OnEmitStatus(int, string) int
}

// RunLoop starts the main loop for V2Ray.
func (v *V2RayPoint) RunLoop(prefIPv6 bool) error {
	v.v2rayOP.Lock()
	defer v.v2rayOP.Unlock()

	if !v.IsRunning {
		v.closeChan = make(chan struct{})
		v.dialer.PrepareResolveChan()

		go func() {
			select {
			case <-v.dialer.ResolveChan():
				if !v.dialer.IsVServerReady() {
					log.Println("vServer cannot be resolved, shutting down")
					v.StopLoop()
					v.SupportSet.Shutdown()
				}
			case <-v.closeChan:
			}
		}()

		if v.AsyncResolve {
			go func() {
				v.dialer.PrepareDomain(v.DomainName, v.closeChan, prefIPv6)
				close(v.dialer.ResolveChan())
			}()
		} else {
			v.dialer.PrepareDomain(v.DomainName, v.closeChan, prefIPv6)
			close(v.dialer.ResolveChan())
		}

		return v.pointloop()
	}
	return nil
}

// StopLoop stops the main loop for V2Ray.
func (v *V2RayPoint) StopLoop() error {
	v.v2rayOP.Lock()
	defer v.v2rayOP.Unlock()

	if v.IsRunning {
		close(v.closeChan)
		v.shutdownInit()
		v.SupportSet.OnEmitStatus(0, "Closed")
	}
	return nil
}

// QueryStats retrieves statistics based on the provided tag and direct.
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

func (v *V2RayPoint) shutdownInit() {
	v.IsRunning = false
	if v.Vpoint != nil {
	    v.Vpoint.Close()
	    v.Vpoint = nil
    }
    v.statsManager = nil
}

func (v *V2RayPoint) pointloop() error {
	log.Println("loading core config")
	config, err := v2serial.LoadJSONConfig(strings.NewReader(v.ConfigureFileContent))
	if err != nil {
	    log.Println(err)
	    return err
    }

	log.Println("new core")
	v.Vpoint, err = v2core.New(config)
	if err != nil {
	    v.Vpoint = nil
	    log.Println(err)
	    return err
    }

    v.statsManager = v.Vpoint.GetFeature(v2stats.ManagerType()).(v2stats.Manager)

	log.Println("start core")
	v.IsRunning = true

	if err := v.Vpoint.Start(); err != nil {
	    v.IsRunning = false
	    log.Println(err)
	    return err
    }

    v.SupportSet.Prepare()
    v.SupportSet.Setup("")
    v.SupportSet.OnEmitStatus(0, "Running")
    return nil
}

func (v *V2RayPoint) MeasureDelay(url string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	go func() {
	    select {
	    case <-v.closeChan:
	        cancel() // Cancel request if close called during measure.
	    case <-ctx.Done():
	    }
    }()

	return measureInstDelay(ctx, v.Vpoint, url)
}

// InitV2Env sets the V2 asset path.
func InitV2Env(envPath string, key string) {
	if len(envPath) > 0 {
	    os.Setenv(v2Asset, envPath)
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

// MeasureOutboundDelay measures the outbound delay for a given URL.
func MeasureOutboundDelay(ConfigureFileContent string, url string) (int64, error) {
	config, err := v2serial.LoadJSONConfig(strings.NewReader(ConfigureFileContent))
	if err != nil {
	    return -1, err 
    }

	config.Inbound = nil // Don't listen to anything for test purpose

	config.App = config.App[:5] // Keep only basic features

	inst, err := v2core.New(config)
	if err != nil { 
	    return -1, err 
    }

	inst.Start()
	delay, err := measureInstDelay(context.Background(), inst, url)
	inst.Close()
	return delay, err 
}

// NewV2RayPoint creates a new V2RayPoint.
func NewV2RayPoint(s V2RayVPNServiceSupportsSet, adns bool) *V2RayPoint { 
    // Inject our own log writer.
	v2applog.RegisterHandlerCreator(v2applog.LogType_Console,
	func(lt v2applog.LogType, options v2applog.HandlerCreatorOptions) (v2commlog.Handler, error) { 
        return v2commlog.NewLogger(createStdoutLogWriter()), nil 
    })

	dialer := NewProtectedDialer(s)

	v2internet.UseAlternativeSystemDialer(dialer)

	return &V2RayPoint{
        SupportSet:   s,
        dialer:       dialer,
        AsyncResolve: adns,
    }
}

// CheckVersionX returns the libv2ray binding version and the Xray version used.
func CheckVersionX() string { 
	var version = 27 
	return fmt.Sprintf("Lib v%d, Xray-core v%s", version, v2core.Version()) 
}

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

	c := &http.Client{
	    Transport: tr,
	    Timeout:   12 * time.Second,
    }

	if len(url) <= 0 { 
	    url = "https://www.google.com/generate_204" 
    }

	req, _ := http.NewRequestWithContext(ctx, "GET", url ,nil)

	start := time.Now()
	resp ,err := c.Do(req)

	if err != nil { 
	    return -1 ,err 
    }

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent { 
	    return -1 ,fmt.Errorf("status != 20x: %s", resp.Status) 
    }

	resp.Body.Close()
	return time.Since(start).Milliseconds(), nil 
}

// consoleLogWriter creates a custom log writer without datetime stamps.
type consoleLogWriter struct { 
	logger *log.Logger 
}

func (w *consoleLogWriter) Write(s string) error { 
	w.logger.Print(s) 
	return nil 
}

func (w *consoleLogWriter) Close() error { 
	return nil 
}

// createStdoutLogWriter creates a logger that won't print date/time stamps.
func createStdoutLogWriter() v2commlog.WriterCreator { 
	return func() v2commlog.Writer { 
        return &consoleLogWriter{ logger: log.New(os.Stdout , "", 0)} 
    } 
}
