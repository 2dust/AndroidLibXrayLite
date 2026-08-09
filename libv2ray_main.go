package libv2ray

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	coreapplog "github.com/xtls/xray-core/app/log"
	coreobservatory "github.com/xtls/xray-core/app/observatory"
	corecommlog "github.com/xtls/xray-core/common/log"
	corenet "github.com/xtls/xray-core/common/net"
	corefilesystem "github.com/xtls/xray-core/common/platform/filesystem"
	"github.com/xtls/xray-core/common/serial"
	core "github.com/xtls/xray-core/core"
	coreextension "github.com/xtls/xray-core/features/extension"
	corerouting "github.com/xtls/xray-core/features/routing"
	corestats "github.com/xtls/xray-core/features/stats"
	coreserial "github.com/xtls/xray-core/infra/conf/serial"
	_ "github.com/xtls/xray-core/main/distro/all"
	browser_dialer "github.com/xtls/xray-core/transport/internet/browser_dialer"
	mobasset "golang.org/x/mobile/asset"
)

// Constants for environment variables
const (
	coreAsset                    = "xray.location.asset"
	coreCert                     = "xray.location.cert"
	xudpBaseKey                  = "xray.xudp.basekey"
	tunFdKey                     = "xray.tun.fd"
	browserDialerAddress         = "xray.browser.dialer"
	libVersion                   = 40 // Library version, update here only
	defaultRealDelayTimeout      = 5 * time.Second
	probeResultAggregationWindow = 50 * time.Millisecond
)

// ProbeHandler receives a group update whenever one of its targets finishes.
// Calls are serialized even though the underlying checks run concurrently.
type ProbeHandler interface {
	OnProbeResult(groupID string, delay int64, completed bool)
}

type probeGroup struct {
	GUID         string   `json:"guid"`
	OutboundTags []string `json:"outboundTags"`
	BalancerTag  string   `json:"balancerTag"`
}

// ProbeController owns one cancellable probe sequence.
type ProbeController struct {
	access    sync.Mutex
	cancel    context.CancelFunc
	cancelled bool
}

func NewProbeController() *ProbeController {
	return &ProbeController{}
}

func (c *ProbeController) Cancel() {
	c.access.Lock()
	c.cancelled = true
	cancel := c.cancel
	c.access.Unlock()
	if cancel != nil {
		cancel()
	}
}

// CoreController represents a controller for managing Xray core instance lifecycle
type CoreController struct {
	CallbackHandler CoreCallbackHandler
	statsManager    corestats.Manager
	coreMutex       sync.Mutex
	coreInstance    *core.Instance
	IsRunning       bool
}

// CoreCallbackHandler defines interface for receiving callbacks and notifications from the core service
type CoreCallbackHandler interface {
	Startup() int
	Shutdown() int
	OnEmitStatus(int, string) int
}

// consoleLogWriter implements a log writer without datetime stamps
// as Android system already adds timestamps to each log line
type consoleLogWriter struct {
	logger *log.Logger // Standard logger
}

// setEnvVariable safely sets an environment variable and logs any errors encountered.
func setEnvVariable(key, value string) {
	if err := os.Setenv(key, value); err != nil {
		log.Printf("Failed to set environment variable %s: %v. Please check your configuration.", key, err)
	}
}

// InitCoreEnv initializes environment variables and file system handlers for the core
// It sets up asset path, certificate path, XUDP base key and customizes the file reader
// to support Android asset system
func InitCoreEnv(envPath string, key string) {
	// Set asset/cert paths
	if len(envPath) > 0 {
		setEnvVariable(coreAsset, envPath)
		setEnvVariable(coreCert, envPath)
	}

	// Set XUDP encryption key
	if len(key) > 0 {
		setEnvVariable(xudpBaseKey, key)
	}

	// Custom file reader with path validation
	corefilesystem.NewFileReader = func(path string) (io.ReadCloser, error) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			_, file := filepath.Split(path)
			return mobasset.Open(file)
		}
		return os.Open(path)
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
		log.Printf("Failed to register log handler: %v", err)
	}

	return &CoreController{
		CallbackHandler: s,
	}
}

// StartLoop initializes and starts the core processing loop
// Thread-safe method that configures and runs the Xray core with the provided configuration
// Returns immediately if the core is already running
func (x *CoreController) StartLoop(configContent string, tunFd int32) (err error) {
	// Set TUN fd key, 0 means do not use TUN
	setEnvVariable(tunFdKey, strconv.Itoa(int(tunFd)))

	x.coreMutex.Lock()
	defer x.coreMutex.Unlock()

	if x.IsRunning {
		log.Println("Core is already running")
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
		x.CallbackHandler.OnEmitStatus(0, "Core stopped")
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

// QueryAllOutboundTrafficStats retrieves and resets all outbound traffic counters.
// Returns a single-line text in format: tag,direction,value;tag,direction,value;
// Returns an empty string if the stats manager is not initialized or no counters exist.
func (x *CoreController) QueryAllOutboundTrafficStats() string {
	if x.statsManager == nil {
		return ""
	}

	var b strings.Builder

	x.statsManager.VisitCounters(func(name string, counter corestats.Counter) bool {
		parts := strings.Split(name, ">>>")
		if len(parts) != 4 || parts[0] != "outbound" || parts[2] != "traffic" {
			return true
		}

		tag := parts[1]
		direct := parts[3]
		value := counter.Set(0)
		if value <= 0 {
			return true // Skip counters with non-positive values
		}

		b.WriteString(tag)
		b.WriteByte(',')
		b.WriteString(direct)
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(value, 10))
		b.WriteByte(';')
		return true
	})
	return b.String()
}

// MeasureDelay measures network latency to a specified URL through the current core instance
// Uses a 12-second timeout context and returns the round-trip time in milliseconds
// An error is returned if the connection fails or returns an unexpected status
func (x *CoreController) MeasureDelay(url string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	return measureInstDelay(ctx, x.coreInstance, url)
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
	ctx, cancel := context.WithTimeout(context.Background(), defaultRealDelayTimeout)
	defer cancel()
	return measureInstDelayWithOptions(ctx, inst, url, http.MethodHead, 1, defaultRealDelayTimeout)
}

// Probe runs all delay-test groups through one short-lived Xray instance.
// Every target is checked once. maxConcurrency limits active Observatory
// checks across every group member.
func (c *ProbeController) Probe(
	configContent, groupsJSON string,
	maxConcurrency int32,
	handler ProbeHandler,
) (err error) {
	// Keep malformed core state on the ordinary error path so the app can isolate
	// the responsible profile instead of losing every result in the batch.
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("probe panicked: %v", value)
		}
	}()
	var groups []probeGroup
	if err := json.Unmarshal([]byte(groupsJSON), &groups); err != nil {
		return fmt.Errorf("probe groups are invalid: %w", err)
	}

	c.access.Lock()
	if c.cancelled {
		c.access.Unlock()
		return context.Canceled
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.access.Unlock()
	defer cancel()

	config, err := coreserial.LoadJSONConfig(strings.NewReader(configContent))
	if err != nil {
		return fmt.Errorf("probe config load failed: %w", err)
	}
	config.Inbound = nil

	inst, err := core.NewWithContext(ctx, config)
	if err != nil {
		return fmt.Errorf("probe instance creation failed: %w", err)
	}
	defer inst.Close()

	burst, ok := inst.GetFeature(coreextension.ObservatoryType()).(coreextension.BurstObservatory)
	if !ok || burst == nil {
		return errors.New("probe burst observatory is unavailable")
	}
	if err := inst.Start(); err != nil {
		return fmt.Errorf("probe startup failed: %w", err)
	}

	return runProbeGroups(
		ctx,
		inst,
		burst,
		groups,
		int(maxConcurrency),
		handler,
	)
}

type probeTarget struct {
	groupIndex  int
	outboundTag string
}

func runProbeGroups(
	ctx context.Context,
	inst *core.Instance,
	burst coreextension.BurstObservatory,
	groups []probeGroup,
	maxConcurrency int,
	handler ProbeHandler,
) error {
	targetCount := 0
	maxGroupSize := 0
	for _, group := range groups {
		targetCount += len(group.OutboundTags)
		if len(group.OutboundTags) > maxGroupSize {
			maxGroupSize = len(group.OutboundTags)
		}
	}
	if targetCount == 0 {
		return nil
	}
	workerCount := maxConcurrency
	if workerCount < 1 {
		workerCount = 1
	}
	if workerCount > targetCount {
		workerCount = targetCount
	}
	jobs := make(chan probeTarget, workerCount)
	completed := make(chan probeTarget, workerCount)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for target := range jobs {
				if ctx.Err() != nil {
					return
				}
				burst.Check([]string{target.outboundTag})
				select {
				case completed <- target:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		// Interleave groups so a large policy group cannot put every other
		// profile behind all of its candidates when concurrency is limited.
		for memberIndex := 0; memberIndex < maxGroupSize; memberIndex++ {
			for groupIndex, group := range groups {
				if memberIndex >= len(group.OutboundTags) {
					continue
				}
				select {
				case jobs <- probeTarget{groupIndex, group.OutboundTags[memberIndex]}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	go func() {
		workers.Wait()
		close(completed)
	}()

	remaining := make([]int, len(groups))
	for index, group := range groups {
		remaining[index] = len(group.OutboundTags)
	}
	for {
		first, ok := <-completed
		if !ok {
			break
		}
		batch, closed := collectProbeCompletions(first, completed, workerCount)
		statuses := currentProbeStatuses(burst)
		results := make(map[int]int64, len(batch))
		for _, target := range batch {
			if _, found := results[target.groupIndex]; !found {
				results[target.groupIndex] = currentProbeDelay(inst, groups[target.groupIndex], statuses)
			}
		}
		for _, target := range batch {
			remaining[target.groupIndex]--
			group := groups[target.groupIndex]
			handler.OnProbeResult(
				group.GUID,
				results[target.groupIndex],
				remaining[target.groupIndex] == 0,
			)
		}
		if closed {
			break
		}
	}
	return ctx.Err()
}

func collectProbeCompletions(
	first probeTarget,
	completed <-chan probeTarget,
	limit int,
) ([]probeTarget, bool) {
	batch := []probeTarget{first}
	timer := time.NewTimer(probeResultAggregationWindow)
	defer timer.Stop()
	for len(batch) < limit {
		select {
		case target, ok := <-completed:
			if !ok {
				return batch, true
			}
			batch = append(batch, target)
		case <-timer.C:
			return batch, false
		}
	}
	return batch, false
}

func currentProbeStatuses(observer coreextension.BurstObservatory) map[string]*coreobservatory.OutboundStatus {
	message, err := observer.GetObservation(context.Background())
	if err != nil {
		return nil
	}
	result, ok := message.(*coreobservatory.ObservationResult)
	if !ok {
		return nil
	}
	statuses := make(map[string]*coreobservatory.OutboundStatus, len(result.GetStatus()))
	for _, status := range result.GetStatus() {
		statuses[status.GetOutboundTag()] = status
	}
	return statuses
}

func currentProbeDelay(
	inst *core.Instance,
	group probeGroup,
	statuses map[string]*coreobservatory.OutboundStatus,
) int64 {
	if len(group.OutboundTags) == 0 {
		return -1
	}
	target := group.OutboundTags[0]
	if group.BalancerTag != "" {
		principle, ok := inst.GetFeature(corerouting.RouterType()).(corerouting.BalancerPrincipleTarget)
		if !ok || principle == nil {
			return -1
		}
		targets, err := principle.GetPrincipleTarget(group.BalancerTag)
		if err != nil || len(targets) == 0 || targets[0] == "" {
			return -1
		}
		target = targets[0]
	}
	status := statuses[target]
	if status != nil && status.GetAlive() {
		return status.GetDelay()
	}
	return -1
}

// CheckVersionX returns the library and Xray versions
func CheckVersionX() string {
	return fmt.Sprintf("Lib v%d, Xray-core v%s", libVersion, core.Version())
}

// ReconcileBrowserDialer updates the browser dialer address and reloads its configuration
// If the dialer address is empty, it will disable the browser dialer and close existing connections
func ReconcileBrowserDialer(dialerAddr string) {
	setEnvVariable(browserDialerAddress, dialerAddr)
	browser_dialer.Reload()
}

// doShutdown shuts down the Xray instance and cleans up resources
func (x *CoreController) doShutdown() {
	if x.coreInstance != nil {
		if err := x.coreInstance.Close(); err != nil {
			log.Printf("core shutdown error: %v", err)
		}
		x.coreInstance = nil
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

	x.coreInstance, err = core.New(config)
	if err != nil {
		return fmt.Errorf("core init failed: %w", err)
	}
	x.statsManager = x.coreInstance.GetFeature(corestats.ManagerType()).(corestats.Manager)

	log.Println("starting core...")
	x.IsRunning = true
	if err := x.coreInstance.Start(); err != nil {
		x.IsRunning = false
		return fmt.Errorf("startup failed: %w", err)
	}

	x.CallbackHandler.Startup()
	x.CallbackHandler.OnEmitStatus(0, "Started successfully, running")

	log.Println("Starting core successfully")
	return nil
}

// measureInstDelay measures the delay for an instance to a given URL
func measureInstDelay(ctx context.Context, inst *core.Instance, url string) (int64, error) {
	return measureInstDelayWithOptions(ctx, inst, url, http.MethodGet, 2, 12*time.Second)
}

func measureInstDelayWithOptions(
	ctx context.Context,
	inst *core.Instance,
	url, method string,
	attempts int,
	timeout time.Duration,
) (int64, error) {
	if inst == nil {
		return -1, errors.New("core instance is nil")
	}

	if url == "" {
		url = "https://www.google.com/generate_204"
	}

	tr := &http.Transport{
		TLSHandshakeTimeout: 6 * time.Second,
		DisableKeepAlives:   false,
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
		Timeout:   timeout,
	}

	var minDuration int64 = -1
	success := false
	var lastErr error

	// Close idle connections to ensure the temporary instance can be closed safely
	defer tr.CloseIdleConnections()

	for i := 0; i < attempts; i++ {
		select {
		case <-ctx.Done():
			// Return immediately when context is canceled
			if !success {
				return -1, ctx.Err()
			}
			return minDuration, nil
		default:
			// Continue execution
		}

		req, err := http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			lastErr = fmt.Errorf("failed to create HTTP request: %w", err)
			continue
		}

		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		// Read GET bodies so a subsequent attempt may reuse the connection.
		if method == http.MethodGet {
			_, err = io.Copy(io.Discard, resp.Body)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			lastErr = fmt.Errorf("invalid status: %s", resp.Status)
			continue
		}

		if err != nil {
			lastErr = fmt.Errorf("failed to read response body: %w", err)
			continue
		}

		duration := time.Since(start).Milliseconds()
		if !success || duration < minDuration {
			minDuration = duration
		}

		success = true
	}
	if !success {
		return -1, lastErr
	}
	return minDuration, nil
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
