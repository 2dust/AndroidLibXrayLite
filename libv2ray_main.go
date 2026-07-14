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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	coreapplog "github.com/xtls/xray-core/app/log"
	coreobservatory "github.com/xtls/xray-core/app/observatory"
	coreobservatoryburst "github.com/xtls/xray-core/app/observatory/burst"
	corerouter "github.com/xtls/xray-core/app/router"
	corecommlog "github.com/xtls/xray-core/common/log"
	corenet "github.com/xtls/xray-core/common/net"
	corefilesystem "github.com/xtls/xray-core/common/platform/filesystem"
	"github.com/xtls/xray-core/common/serial"
	core "github.com/xtls/xray-core/core"
	coreextension "github.com/xtls/xray-core/features/extension"
	coreoutbound "github.com/xtls/xray-core/features/outbound"
	corerouting "github.com/xtls/xray-core/features/routing"
	corestats "github.com/xtls/xray-core/features/stats"
	coreserial "github.com/xtls/xray-core/infra/conf/serial"
	_ "github.com/xtls/xray-core/main/distro/all"
	browser_dialer "github.com/xtls/xray-core/transport/internet/browser_dialer"
	mobasset "golang.org/x/mobile/asset"
)

// Constants for environment variables
const (
	coreAsset            = "xray.location.asset"
	coreCert             = "xray.location.cert"
	xudpBaseKey          = "xray.xudp.basekey"
	tunFdKey             = "xray.tun.fd"
	browserDialerAddress = "xray.browser.dialer"
	libVersion           = 42 // Library version, update here only
)

const (
	warmRouteDeadlineGrace     = 2 * time.Second
	defaultObservationDeadline = 5 * time.Second
	balancerTargetPollInterval = 250 * time.Millisecond
)

type routedBalancerPlan struct {
	tag       string
	selectors []string
}

// OutboundProbeHandler receives JSON snapshots while a one-shot outbound probe
// batch is running. Callbacks are serialized and are published after each
// candidate finishes, even while other candidates are still pending. These
// snapshots are non-terminal; the method return carries the final state.
type OutboundProbeHandler interface {
	OnOutboundProbeUpdate(string) int
}

type outboundProbeStatus struct {
	OutboundTag   string `json:"outboundTag"`
	Alive         bool   `json:"alive"`
	Delay         int64  `json:"delay"`
	LastError     string `json:"lastError,omitempty"`
	Samples       int64  `json:"samples"`
	FailedSamples int64  `json:"failedSamples"`
	Deviation     int64  `json:"deviation"`
}

type outboundProbeBatchResult struct {
	Completed          bool                  `json:"completed"`
	Cancelled          bool                  `json:"cancelled,omitempty"`
	NetworkUnavailable bool                  `json:"networkUnavailable,omitempty"`
	Error              string                `json:"error,omitempty"`
	Results            []outboundProbeStatus `json:"results"`
	BalancerTargets    map[string]string     `json:"balancerTargets,omitempty"`
}

// OutboundProbeController owns one finite probe operation. It is separate from
// CoreController because embedders are expected to run it in a short-lived,
// dedicated process rather than beside a long-running proxy core.
type OutboundProbeController struct {
	access  sync.Mutex
	cancel  context.CancelFunc
	running bool
	used    bool
}

// NewOutboundProbeController creates a single-use batch controller. The owning
// process should be discarded after a probe method returns, regardless of its
// result, so a later batch cannot inherit process-wide native state.
func NewOutboundProbeController() *OutboundProbeController {
	return &OutboundProbeController{}
}

// Cancel interrupts the active batch, if any. The probe method will return a
// partial JSON snapshot containing every measurement completed before the
// cancellation was observed.
func (c *OutboundProbeController) Cancel() {
	c.access.Lock()
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
	configContent   string
	stopTargetWatch func()
	warmRouteTimer  *time.Timer
	IsRunning       bool
}

// CoreCallbackHandler defines interface for receiving callbacks and notifications from the core service
type CoreCallbackHandler interface {
	Startup() int
	Shutdown() int
	OnEmitStatus(int, string) int
	// OnBalancerTargetChanged acknowledges a fresh target with zero. A nonzero
	// result keeps any warm override active and retries the notification.
	OnBalancerTargetChanged(string, string) int
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

	if err := x.doStartLoop(configContent, "", ""); err != nil {
		return err
	}
	x.configContent = configContent
	return nil
}

// StopLoop safely stops the core processing loop and releases resources
// Thread-safe method that shuts down the core instance and triggers necessary callbacks
func (x *CoreController) StopLoop() error {
	x.coreMutex.Lock()
	defer x.coreMutex.Unlock()

	if x.IsRunning {
		x.doShutdown()
		x.configContent = ""
		x.CallbackHandler.OnEmitStatus(0, "Core stopped")
	}
	return nil
}

// ResetNetworkState recreates the running Xray instance while keeping the
// Android-owned TUN file descriptor open. Recreating the instance closes
// network-bound TCP, UDP, mux and transport-pool state so new connections are
// established on the current Android underlay after Wi-Fi/cellular changes.
func (x *CoreController) ResetNetworkState() error {
	return x.resetNetworkState("", "")
}

// ResetNetworkStateWithWarmRoute recreates the running Xray instance and
// pins a previously viable outbound while the new observatory gathers results.
// The override is removed only after the strategy has a fresh target and the
// callback has acknowledged it. If observation stalls, the known route remains
// active instead of leaving the balancer without a usable outbound.
func (x *CoreController) ResetNetworkStateWithWarmRoute(balancerTag, target string) error {
	return x.resetNetworkState(balancerTag, target)
}

func (x *CoreController) resetNetworkState(balancerTag, target string) error {
	return x.resetNetworkStateWithStarter(balancerTag, target, x.doStartLoop)
}

func (x *CoreController) resetNetworkStateWithStarter(
	balancerTag, target string,
	startLoop func(string, string, string) error,
) error {
	x.coreMutex.Lock()
	defer x.coreMutex.Unlock()

	if !x.IsRunning {
		return nil
	}
	if x.configContent == "" {
		return errors.New("running core configuration is unavailable")
	}

	configContent := x.configContent
	log.Println("resetting core network state...")
	x.doShutdown()
	if resetErr := startLoop(configContent, balancerTag, target); resetErr != nil {
		log.Printf("core network state reset failed, retrying original configuration: %v", resetErr)
		if rollbackErr := startLoop(configContent, "", ""); rollbackErr != nil {
			message := fmt.Sprintf(
				"Core network state reset failed: %v; original configuration recovery failed: %v",
				resetErr,
				rollbackErr,
			)
			x.CallbackHandler.OnEmitStatus(1, message)
			return fmt.Errorf(
				"core network state reset failed: %w; original configuration recovery failed: %v",
				resetErr,
				rollbackErr,
			)
		}

		x.CallbackHandler.OnEmitStatus(0, "Core network state reset recovered using the original configuration")
		log.Println("Core network state reset recovered using the original configuration")
		return nil
	}

	x.CallbackHandler.OnEmitStatus(0, "Core network state reset")
	log.Println("Core network state reset successfully")
	return nil
}

// GetBalancerPrincipleTarget returns the strategy's current first-choice
// outbound. An empty result means the observatory has not produced a viable
// target yet or the running profile has no compatible balancer.
func (x *CoreController) GetBalancerPrincipleTarget(balancerTag string) (string, error) {
	x.coreMutex.Lock()
	defer x.coreMutex.Unlock()

	if !x.IsRunning || x.coreInstance == nil {
		return "", nil
	}
	return firstBalancerPrincipleTarget(x.coreInstance, balancerTag)
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

	// Preserve the established lightweight single-outbound measurement API for
	// existing embedders. Policy-group batch probing uses ProbeOutbounds instead.
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

// ValidateOutboundProbeConfig parses one source configuration without creating
// an Xray instance. Batch builders can use this to reject one malformed source
// before combining it with otherwise viable profiles.
func ValidateOutboundProbeConfig(configContent string) error {
	config, err := coreserial.LoadJSONConfig(strings.NewReader(configContent))
	if err != nil {
		return fmt.Errorf("outbound probe config load failed: %w", err)
	}
	if len(config.Outbound) == 0 {
		return errors.New("outbound probe config has no outbounds")
	}
	return nil
}

// ProbeOutbounds runs a finite, bounded outbound probe batch through one Xray
// instance. Each outbound tag is treated as an independent concurrency group.
// Existing embedders retain their original API while ProbeOutboundGroups lets
// callers preserve higher-level configuration boundaries.
func (c *OutboundProbeController) ProbeOutbounds(
	configContent, outboundTagsJSON, balancerTagsJSON string,
	maxConcurrency, samples int32,
	handler OutboundProbeHandler,
) (string, error) {
	return c.probeOutbounds(
		configContent,
		outboundTagsJSON,
		balancerTagsJSON,
		maxConcurrency,
		samples,
		handler,
		false,
	)
}

// ProbeOutboundGroups runs one probe task per JSON tag array. At most
// maxConcurrency groups are active at once. Members of one group are checked
// concurrently, and a progressive snapshot is published as each member
// finishes. This matches v2rayNG's configuration-level concurrency semantics
// while allowing a policy-group result to improve before its slowest member
// reaches the timeout.
func (c *OutboundProbeController) ProbeOutboundGroups(
	configContent, outboundGroupsJSON, balancerTagsJSON string,
	maxConcurrency, samples int32,
	handler OutboundProbeHandler,
) (string, error) {
	return c.probeOutbounds(
		configContent,
		outboundGroupsJSON,
		balancerTagsJSON,
		maxConcurrency,
		samples,
		handler,
		true,
	)
}

func (c *OutboundProbeController) probeOutbounds(
	configContent, outboundWorkJSON, balancerTagsJSON string,
	maxConcurrency, samples int32,
	handler OutboundProbeHandler,
	grouped bool,
) (string, error) {
	c.access.Lock()
	if c.running {
		c.access.Unlock()
		return "", errors.New("an outbound probe batch is already running")
	}
	if c.used {
		c.access.Unlock()
		return "", errors.New("outbound probe controller is single-use")
	}
	baseCtx, cancel := context.WithCancel(context.Background())
	c.used = true
	c.running = true
	c.cancel = cancel
	c.access.Unlock()
	defer func() {
		cancel()
		c.access.Lock()
		c.cancel = nil
		c.running = false
		c.access.Unlock()
	}()

	outboundGroups, err := decodeOutboundProbeGroups(outboundWorkJSON, grouped)
	if err != nil {
		return "", err
	}
	if maxConcurrency <= 0 {
		return "", errors.New("outbound probe concurrency must be positive")
	}
	if samples <= 0 {
		return "", errors.New("outbound probe sample count must be positive")
	}
	var balancerTags []string
	if err := json.Unmarshal([]byte(balancerTagsJSON), &balancerTags); err != nil {
		return "", fmt.Errorf("outbound probe balancer tags are invalid: %w", err)
	}

	config, err := coreserial.LoadJSONConfig(strings.NewReader(configContent))
	if err != nil {
		return "", fmt.Errorf("outbound probe config load failed: %w", err)
	}
	// Inbounds are never valid in the disposable probing process. Keep the
	// remaining application features intact: silently filtering them here can
	// break otherwise valid third-party outbound configurations.
	config.Inbound = nil

	inst, err := core.New(config)
	if err != nil {
		return "", fmt.Errorf("outbound probe instance creation failed: %w", err)
	}
	defer inst.Close()

	feature := inst.GetFeature(coreextension.ObservatoryType())
	batch, ok := feature.(coreextension.BurstObservatory)
	if !ok {
		return "", errors.New("outbound probe config does not contain a burst observatory")
	}

	if err := inst.Start(); err != nil {
		return "", fmt.Errorf("outbound probe startup failed: %w", err)
	}

	probeErr := runUpstreamBurstProbeGroups(
		baseCtx,
		batch,
		outboundGroups,
		int(maxConcurrency),
		int(samples),
		func() error {
			_, err := outboundProbeSnapshot(inst, balancerTags, nil, false, handler)
			return err
		},
	)
	return outboundProbeSnapshot(inst, balancerTags, probeErr, true, nil)
}

func decodeOutboundProbeGroups(encoded string, grouped bool) ([][]string, error) {
	var groups [][]string
	if grouped {
		if err := json.Unmarshal([]byte(encoded), &groups); err != nil {
			return nil, fmt.Errorf("outbound probe groups are invalid: %w", err)
		}
	} else {
		var tags []string
		if err := json.Unmarshal([]byte(encoded), &tags); err != nil {
			return nil, fmt.Errorf("outbound probe tags are invalid: %w", err)
		}
		groups = make([][]string, 0, len(tags))
		for _, tag := range tags {
			groups = append(groups, []string{tag})
		}
	}
	if len(groups) == 0 {
		return nil, errors.New("outbound probe groups are empty")
	}

	seen := make(map[string]struct{})
	for groupIndex, group := range groups {
		if len(group) == 0 {
			return nil, fmt.Errorf("outbound probe group %d is empty", groupIndex)
		}
		for _, tag := range group {
			if tag == "" {
				return nil, errors.New("outbound probe groups contain an empty tag")
			}
			if _, exists := seen[tag]; exists {
				return nil, fmt.Errorf("outbound probe tag %q is duplicated", tag)
			}
			seen[tag] = struct{}{}
		}
	}
	return groups, nil
}

// runUpstreamBurstChecks preserves the original flat-tag adapter API. Each tag
// is an independent group, so maxConcurrency bounds simultaneous tag checks.
func runUpstreamBurstChecks(
	ctx context.Context,
	observer coreextension.BurstObservatory,
	outboundTags []string,
	maxConcurrency, samples int,
	afterProbe func() error,
) error {
	groups := make([][]string, 0, len(outboundTags))
	for _, tag := range outboundTags {
		groups = append(groups, []string{tag})
	}
	return runUpstreamBurstProbeGroups(ctx, observer, groups, maxConcurrency, samples, afterProbe)
}

// runUpstreamBurstProbeGroups adapts upstream's blocking Check API without
// losing progressive results. A worker owns one configuration group until all
// of its samples finish; group members use concurrent single-tag Check calls.
// The coordinator serializes afterProbe callbacks as individual members return.
func runUpstreamBurstProbeGroups(
	ctx context.Context,
	observer coreextension.BurstObservatory,
	outboundGroups [][]string,
	maxConcurrency, samples int,
	afterProbe func() error,
) error {
	if maxConcurrency <= 0 {
		return errors.New("outbound probe concurrency must be positive")
	}
	if samples <= 0 {
		return errors.New("outbound probe sample count must be positive")
	}
	if len(outboundGroups) == 0 {
		return errors.New("outbound probe groups are empty")
	}

	jobs := make(chan []string, len(outboundGroups))
	for groupIndex, group := range outboundGroups {
		if len(group) == 0 {
			return fmt.Errorf("outbound probe group %d is empty", groupIndex)
		}
		copied := append([]string(nil), group...)
		jobs <- copied
	}
	close(jobs)

	type completion struct {
		checked      bool
		acknowledged chan struct{}
	}
	completed := make(chan completion)
	workerCount := maxConcurrency
	if workerCount > len(outboundGroups) {
		workerCount = len(outboundGroups)
	}
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for worker := 0; worker < workerCount; worker++ {
		go func() {
			defer workers.Done()
			for group := range jobs {
				for sample := 0; sample < samples; sample++ {
					var members sync.WaitGroup
					members.Add(len(group))
					for _, outboundTag := range group {
						tag := outboundTag
						go func() {
							defer members.Done()
							result := completion{acknowledged: make(chan struct{})}
							if ctx.Err() != nil {
								completed <- result
								<-result.acknowledged
								return
							}
							observer.Check([]string{tag})
							result.checked = true
							completed <- result
							<-result.acknowledged
						}()
					}
					members.Wait()
				}
			}
		}()
	}
	go func() {
		workers.Wait()
		close(completed)
	}()

	var callbackErr error
	for result := range completed {
		if result.checked && callbackErr == nil && afterProbe != nil {
			callbackErr = afterProbe()
		}
		close(result.acknowledged)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return callbackErr
}

func outboundProbeSnapshot(
	inst *core.Instance,
	balancerTags []string,
	probeErr error,
	final bool,
	handler OutboundProbeHandler,
) (string, error) {
	feature := inst.GetFeature(coreextension.ObservatoryType())
	observer, ok := feature.(coreextension.Observatory)
	if !ok {
		return "", errors.New("outbound probe observatory is unavailable")
	}
	message, err := observer.GetObservation(context.Background())
	if err != nil {
		return "", fmt.Errorf("outbound probe result query failed: %w", err)
	}
	result, ok := message.(*coreobservatory.ObservationResult)
	if !ok {
		return "", errors.New("unexpected outbound probe result type")
	}

	statuses := make([]outboundProbeStatus, 0, len(result.GetStatus()))
	for _, status := range result.GetStatus() {
		health := status.GetHealthPing()
		statuses = append(statuses, outboundProbeStatus{
			OutboundTag:   status.GetOutboundTag(),
			Alive:         status.GetAlive(),
			Delay:         status.GetDelay(),
			LastError:     status.GetLastErrorReason(),
			Samples:       health.GetAll(),
			FailedSamples: health.GetFail(),
			Deviation:     health.GetDeviation(),
		})
	}
	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].OutboundTag < statuses[j].OutboundTag
	})

	targets := make(map[string]string)
	for _, balancerTag := range balancerTags {
		target, targetErr := firstBalancerPrincipleTarget(inst, balancerTag)
		if targetErr == nil && target != "" {
			targets[balancerTag] = target
		}
	}
	if len(targets) == 0 {
		targets = nil
	}

	payload := outboundProbeBatchResult{
		Completed:          final && probeErr == nil,
		Cancelled:          errors.Is(probeErr, context.Canceled),
		NetworkUnavailable: final && probeErr == nil && len(statuses) == 0,
		Results:            statuses,
		BalancerTargets:    targets,
	}
	if probeErr != nil {
		payload.Error = probeErr.Error()
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("outbound probe result encoding failed: %w", err)
	}
	resultJSON := string(encoded)
	if handler != nil {
		handler.OnOutboundProbeUpdate(resultJSON)
	}
	return resultJSON, nil
}

func routedBalancerPlanForConfig(config *core.Config) (routedBalancerPlan, error) {
	hasObservatory := false
	for _, app := range config.App {
		if app.Type == "xray.core.app.observatory.Config" ||
			app.Type == "xray.core.app.observatory.burst.Config" {
			hasObservatory = true
			break
		}
	}
	if !hasObservatory {
		return routedBalancerPlan{}, nil
	}

	for _, app := range config.App {
		if app.Type != "xray.app.router.Config" {
			continue
		}
		instance, err := app.GetInstance()
		if err != nil {
			return routedBalancerPlan{}, err
		}
		routerConfig, ok := instance.(*corerouter.Config)
		if !ok {
			return routedBalancerPlan{}, errors.New("unexpected router config type")
		}
		for _, rule := range routerConfig.GetRule() {
			balancerTag := rule.GetBalancingTag()
			if balancerTag == "" {
				continue
			}
			for _, balancer := range routerConfig.GetBalancingRule() {
				if balancer.GetTag() == balancerTag {
					strategy := balancer.GetStrategy()
					if strategy != "leastPing" && strategy != "leastLoad" {
						return routedBalancerPlan{}, nil
					}
					return routedBalancerPlan{
						tag:       balancerTag,
						selectors: append([]string(nil), balancer.GetOutboundSelector()...),
					}, nil
				}
			}
			return routedBalancerPlan{}, fmt.Errorf("routed balancer %q is not configured", balancerTag)
		}
	}
	return routedBalancerPlan{}, nil
}

func balancerFeatures(inst *core.Instance) (corerouting.BalancerPrincipleTarget, corerouting.BalancerOverrider, error) {
	if inst == nil {
		return nil, nil, errors.New("core instance is nil")
	}
	feature := inst.GetFeature(corerouting.RouterType())
	principle, ok := feature.(corerouting.BalancerPrincipleTarget)
	if !ok {
		return nil, nil, errors.New("router does not expose balancer principle targets")
	}
	overrider, ok := feature.(corerouting.BalancerOverrider)
	if !ok {
		return nil, nil, errors.New("router does not support balancer overrides")
	}
	return principle, overrider, nil
}

func firstBalancerPrincipleTarget(inst *core.Instance, balancerTag string) (string, error) {
	if balancerTag == "" {
		return "", nil
	}
	if inst == nil {
		return "", errors.New("core instance is nil")
	}
	principle, ok := inst.GetFeature(corerouting.RouterType()).(corerouting.BalancerPrincipleTarget)
	if !ok {
		return "", errors.New("router does not expose balancer principle targets")
	}
	targets, err := principle.GetPrincipleTarget(balancerTag)
	if err != nil {
		return "", err
	}
	for _, target := range targets {
		if target != "" {
			return target, nil
		}
	}
	return "", nil
}

func setBalancerOverride(inst *core.Instance, balancerTag, target string) error {
	if balancerTag == "" || target == "" {
		return errors.New("balancer tag and target are required")
	}
	manager, ok := inst.GetFeature(coreoutbound.ManagerType()).(coreoutbound.Manager)
	if !ok || manager.GetHandler(target) == nil {
		return fmt.Errorf("outbound %q is not present", target)
	}
	_, overrider, err := balancerFeatures(inst)
	if err != nil {
		return err
	}
	return overrider.SetOverrideTarget(balancerTag, target)
}

func clearBalancerOverride(inst *core.Instance, balancerTag string) error {
	_, overrider, err := balancerFeatures(inst)
	if err != nil {
		return err
	}
	return overrider.SetOverrideTarget(balancerTag, "")
}

func getBalancerOverride(inst *core.Instance, balancerTag string) (string, error) {
	_, overrider, err := balancerFeatures(inst)
	if err != nil {
		return "", err
	}
	return overrider.GetOverrideTarget(balancerTag)
}

func observationResultDeadlineForProbe(probeDeadline time.Duration) (time.Duration, error) {
	if probeDeadline <= 0 {
		return 0, errors.New("observatory did not report a valid probe deadline")
	}
	return probeDeadline + warmRouteDeadlineGrace, nil
}

func observationResultDeadline(config *core.Config) (time.Duration, error) {
	if config == nil {
		return 0, errors.New("core config is nil")
	}
	for _, app := range config.App {
		instance, err := app.GetInstance()
		if err != nil {
			return 0, fmt.Errorf("observatory config inspection failed: %w", err)
		}
		switch observatoryConfig := instance.(type) {
		case *coreobservatoryburst.Config:
			probeDeadline := defaultObservationDeadline
			if pingConfig := observatoryConfig.GetPingConfig(); pingConfig != nil && pingConfig.GetTimeout() > 0 {
				probeDeadline = time.Duration(pingConfig.GetTimeout())
			}
			return observationResultDeadlineForProbe(probeDeadline)
		case *coreobservatory.Config:
			// The upstream standard Observatory uses a five-second HTTP client
			// timeout for each concurrent initial probe.
			return observationResultDeadlineForProbe(defaultObservationDeadline)
		}
	}
	return 0, errors.New("core config does not contain an observatory")
}

func watchBalancerTargetChanges(inst *core.Instance, balancerTag string, handler CoreCallbackHandler) (func(), error) {
	return watchBalancerTargetChangesWithInterval(inst, balancerTag, handler, balancerTargetPollInterval)
}

func watchBalancerTargetChangesWithInterval(
	inst *core.Instance,
	balancerTag string,
	handler CoreCallbackHandler,
	pollInterval time.Duration,
) (func(), error) {
	if balancerTag == "" {
		return func() {}, nil
	}
	if inst == nil {
		return nil, errors.New("core instance is nil")
	}
	observer := inst.GetFeature(coreextension.ObservatoryType())
	if _, ok := observer.(coreextension.Observatory); !ok {
		return nil, errors.New("observatory is unavailable")
	}
	if _, ok := inst.GetFeature(corerouting.RouterType()).(corerouting.BalancerPrincipleTarget); !ok {
		return nil, errors.New("router does not expose balancer principle targets")
	}
	if pollInterval <= 0 {
		return nil, errors.New("balancer target poll interval must be positive")
	}

	watchCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		lastTarget := ""
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
			}

			lastTarget = handleBalancerTargetChange(inst, balancerTag, lastTarget, handler)
		}
	}()

	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			cancel()
			<-done
		})
	}, nil
}

func handleBalancerTargetChange(
	inst *core.Instance,
	balancerTag, lastTarget string,
	handler CoreCallbackHandler,
) string {
	target, err := firstBalancerPrincipleTarget(inst, balancerTag)
	if err != nil || target == "" {
		return lastTarget
	}
	if target != lastTarget && handler != nil {
		if status := handler.OnBalancerTargetChanged(balancerTag, target); status != 0 {
			log.Printf("fresh balancer target %q was not acknowledged (status %d); retaining warm route", target, status)
			return lastTarget
		}
	}
	if override, err := getBalancerOverride(inst, balancerTag); err != nil {
		log.Printf("failed to inspect warm route before handoff to %q: %v", target, err)
	} else if override != "" {
		if err := clearBalancerOverride(inst, balancerTag); err != nil {
			log.Printf("failed to release warm route %q: %v", override, err)
		} else {
			log.Printf("handed off warm route %q to fresh observatory target %q", override, target)
		}
	}
	return target
}

// reportWarmRouteReadinessDeadline deliberately does not clear the override.
// Upstream probe timeouts are estimates for when a result should be ready, not
// proof that the balancer has a viable replacement. The target watcher remains
// responsible for the eventual acknowledged handoff.
func reportWarmRouteReadinessDeadline(
	inst *core.Instance,
	balancerTag, warmTarget string,
	expectedWithin time.Duration,
) {
	override, err := getBalancerOverride(inst, balancerTag)
	if err != nil || override != warmTarget {
		return
	}
	target, targetErr := firstBalancerPrincipleTarget(inst, balancerTag)
	switch {
	case targetErr != nil:
		log.Printf(
			"warm route %q remains active after %v because the fresh target query failed: %v",
			warmTarget,
			expectedWithin,
			targetErr,
		)
	case target == "":
		log.Printf(
			"warm route %q remains active after %v because observatory has no fresh target",
			warmTarget,
			expectedWithin,
		)
	default:
		log.Printf(
			"warm route %q remains active until the target watcher completes handoff to %q",
			warmTarget,
			target,
		)
	}
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
	if x.warmRouteTimer != nil {
		x.warmRouteTimer.Stop()
		x.warmRouteTimer = nil
	}
	if x.stopTargetWatch != nil {
		x.stopTargetWatch()
		x.stopTargetWatch = nil
	}
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
func (x *CoreController) doStartLoop(configContent, warmBalancerTag, warmTarget string) error {
	log.Println("initializing core...")
	config, err := coreserial.LoadJSONConfig(strings.NewReader(configContent))
	if err != nil {
		return fmt.Errorf("config error: %w", err)
	}
	plan, err := routedBalancerPlanForConfig(config)
	if err != nil {
		return fmt.Errorf("route inspection failed: %w", err)
	}

	instance, err := core.New(config)
	if err != nil {
		return fmt.Errorf("core init failed: %w", err)
	}
	stopTargetWatch, err := watchBalancerTargetChanges(instance, plan.tag, x.CallbackHandler)
	if err != nil {
		_ = instance.Close()
		return fmt.Errorf("balancer target watch failed: %w", err)
	}
	warmRouteApplied := false
	warmRouteLifetime := time.Duration(0)
	if warmTarget != "" {
		warmRouteLifetime, err = observationResultDeadline(config)
		if err != nil {
			log.Printf("warm route %q was not applied: %v", warmTarget, err)
		} else if err := setBalancerOverride(instance, warmBalancerTag, warmTarget); err != nil {
			log.Printf("warm route %q was not applied: %v", warmTarget, err)
		} else {
			log.Printf(
				"using warm route %q while observatory starts; expecting a fresh target within %v",
				warmTarget,
				warmRouteLifetime,
			)
			warmRouteApplied = true
		}
	}

	log.Println("starting core...")
	if err := instance.Start(); err != nil {
		stopTargetWatch()
		_ = instance.Close()
		return fmt.Errorf("startup failed: %w", err)
	}

	x.coreInstance = instance
	x.stopTargetWatch = stopTargetWatch
	x.statsManager = instance.GetFeature(corestats.ManagerType()).(corestats.Manager)
	x.IsRunning = true
	if warmRouteApplied {
		x.warmRouteTimer = time.AfterFunc(warmRouteLifetime, func() {
			x.coreMutex.Lock()
			defer x.coreMutex.Unlock()
			x.warmRouteTimer = nil
			if !x.IsRunning || x.coreInstance != instance {
				return
			}
			reportWarmRouteReadinessDeadline(
				instance,
				warmBalancerTag,
				warmTarget,
				warmRouteLifetime,
			)
		})
	}

	x.CallbackHandler.Startup()
	x.CallbackHandler.OnEmitStatus(0, "Started successfully, running")

	log.Println("Starting core successfully")
	return nil
}

// measureInstDelay measures the delay for an instance to a given URL
func measureInstDelay(ctx context.Context, inst *core.Instance, url string) (int64, error) {
	return measureInstDelayWithOptions(ctx, inst, url, 12*time.Second, 2)
}

func measureInstDelayWithOptions(ctx context.Context, inst *core.Instance, url string, timeout time.Duration, attempts int) (int64, error) {
	if inst == nil {
		return -1, errors.New("core instance is nil")
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

	if url == "" {
		url = "https://www.google.com/generate_204"
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return -1, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	var minDuration int64 = -1
	success := false
	var lastErr error

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

		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		// Ensure response body is closed
		defer func(resp *http.Response) {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
		}(resp)

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			lastErr = fmt.Errorf("invalid status: %s", resp.Status)
			continue
		}

		// Handle possible errors when reading response body
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
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
