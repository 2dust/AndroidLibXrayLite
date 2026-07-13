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

const warmRouteDeadlineGrace = 2 * time.Second

type routedBalancerPlan struct {
	tag       string
	selectors []string
}

type outboundDelayResult struct {
	Delay       int64  `json:"delay"`
	OutboundTag string `json:"outboundTag,omitempty"`
}

// OutboundProbeHandler receives coalesced JSON snapshots while a one-shot
// outbound probe batch is running. Callbacks are serialized and may contain
// more than one newly completed outbound when native updates were coalesced.
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
// process should be discarded after ProbeOutbounds returns, regardless of its
// result, so a later batch cannot inherit process-wide native state.
func NewOutboundProbeController() *OutboundProbeController {
	return &OutboundProbeController{}
}

// Cancel interrupts the active batch, if any. ProbeOutbounds will return a
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
// temporarily pins a previously viable outbound while the new observatory
// gathers results. The override is removed as soon as the strategy has a
// fresh target, or after a bounded timeout.
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
	result, err := measureOutboundDelay(ConfigureFileContent, url, "")
	return result.Delay, err
}

// MeasureOutboundDelayWithWarmRoute measures a temporary policy-group core
// through a previously viable outbound when one is available. The JSON result
// includes the outbound that succeeded so the caller can refresh its cache.
func MeasureOutboundDelayWithWarmRoute(ConfigureFileContent, url, warmTarget string) (string, error) {
	result, err := measureOutboundDelay(ConfigureFileContent, url, warmTarget)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("delay result encoding failed: %w", err)
	}
	return string(payload), nil
}

// ProbeOutbounds runs a finite, concurrent outbound probe batch through one
// Xray instance. outboundTagsJSON and balancerTagsJSON are JSON string arrays.
// The returned JSON always preserves partial runtime results; probe failures,
// cancellation, and network loss are represented in that payload rather than
// discarding successful measurements through a gomobile exception.
func (c *OutboundProbeController) ProbeOutbounds(
	configContent, outboundTagsJSON, balancerTagsJSON string,
	maxConcurrency, samples int32,
	handler OutboundProbeHandler,
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

	var outboundTags []string
	if err := json.Unmarshal([]byte(outboundTagsJSON), &outboundTags); err != nil {
		return "", fmt.Errorf("outbound probe tags are invalid: %w", err)
	}
	var balancerTags []string
	if err := json.Unmarshal([]byte(balancerTagsJSON), &balancerTags); err != nil {
		return "", fmt.Errorf("outbound probe balancer tags are invalid: %w", err)
	}

	config, err := coreserial.LoadJSONConfig(strings.NewReader(configContent))
	if err != nil {
		return "", fmt.Errorf("outbound probe config load failed: %w", err)
	}
	config.Inbound = nil
	var essentialApp []*serial.TypedMessage
	for _, app := range config.App {
		if app.Type == "xray.app.proxyman.OutboundConfig" ||
			app.Type == "xray.app.dispatcher.Config" ||
			app.Type == "xray.app.log.Config" ||
			app.Type == "xray.app.router.Config" ||
			app.Type == "xray.core.app.observatory.burst.Config" {
			essentialApp = append(essentialApp, app)
		}
	}
	config.App = essentialApp

	inst, err := core.New(config)
	if err != nil {
		return "", fmt.Errorf("outbound probe instance creation failed: %w", err)
	}
	defer inst.Close()

	feature := inst.GetFeature(coreextension.ObservatoryType())
	batch, ok := feature.(coreextension.ObservatoryBatchProbe)
	if !ok {
		return "", errors.New("burst observatory does not support one-shot outbound probing")
	}
	notifier, ok := feature.(coreextension.ObservatoryUpdateNotifier)
	if !ok {
		return "", errors.New("outbound probe observatory does not publish updates")
	}
	deadline, err := batch.ProbeOutboundsDeadline(outboundTags, int(maxConcurrency), int(samples))
	if err != nil {
		return "", fmt.Errorf("outbound probe deadline is invalid: %w", err)
	}
	if deadline > time.Duration(1<<63-1)-warmRouteDeadlineGrace {
		return "", errors.New("outbound probe deadline exceeds time.Duration")
	}

	updates, unsubscribe := notifier.SubscribeObservationUpdates()
	defer unsubscribe()
	if err := inst.Start(); err != nil {
		return "", fmt.Errorf("outbound probe startup failed: %w", err)
	}

	probeCtx := baseCtx
	deadlineCancel := func() {}
	if deadline > 0 {
		probeCtx, deadlineCancel = context.WithTimeout(baseCtx, deadline+warmRouteDeadlineGrace)
	}
	defer deadlineCancel()

	probeDone := make(chan error, 1)
	go func() {
		probeDone <- batch.ProbeOutbounds(
			probeCtx,
			outboundTags,
			int(maxConcurrency),
			int(samples),
		)
	}()

	for {
		select {
		case _, open := <-updates:
			if !open {
				cancel()
				probeErr := <-probeDone
				if probeErr == nil {
					probeErr = errors.New("outbound probe observatory closed before completion")
				}
				return outboundProbeSnapshot(inst, balancerTags, probeErr, handler)
			}
			if _, err := outboundProbeSnapshot(inst, balancerTags, nil, handler); err != nil {
				cancel()
				<-probeDone
				return "", err
			}
		case probeErr := <-probeDone:
			return outboundProbeSnapshot(inst, balancerTags, probeErr, handler)
		}
	}
}

func outboundProbeSnapshot(
	inst *core.Instance,
	balancerTags []string,
	probeErr error,
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
		Completed:          probeErr == nil,
		Cancelled:          errors.Is(probeErr, context.Canceled),
		NetworkUnavailable: errors.Is(probeErr, coreextension.ErrObservatoryProbeNetworkUnavailable),
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

func measureOutboundDelay(ConfigureFileContent, url, warmTarget string) (outboundDelayResult, error) {
	failure := outboundDelayResult{Delay: -1}
	config, err := coreserial.LoadJSONConfig(strings.NewReader(ConfigureFileContent))
	if err != nil {
		return failure, fmt.Errorf("config load error: %w", err)
	}
	plan, err := routedBalancerPlanForConfig(config)
	if err != nil {
		return failure, fmt.Errorf("speed-test route inspection failed: %w", err)
	}

	// Simplify config for testing
	config.Inbound = nil
	var essentialApp []*serial.TypedMessage
	for _, app := range config.App {
		if app.Type == "xray.app.proxyman.OutboundConfig" ||
			app.Type == "xray.app.dispatcher.Config" ||
			app.Type == "xray.app.log.Config" ||
			app.Type == "xray.app.router.Config" ||
			app.Type == "xray.core.app.observatory.Config" ||
			app.Type == "xray.core.app.observatory.burst.Config" {
			essentialApp = append(essentialApp, app)
		}
	}
	config.App = essentialApp

	inst, err := core.New(config)
	if err != nil {
		return failure, fmt.Errorf("instance creation failed: %w", err)
	}

	if err := inst.Start(); err != nil {
		return failure, fmt.Errorf("startup failed: %w", err)
	}
	defer inst.Close()

	if warmTarget != "" && plan.tag != "" {
		if err := setBalancerOverride(inst, plan.tag, warmTarget); err == nil {
			log.Printf("testing policy group through warm route %q", warmTarget)
			delay, warmErr := measureInstDelayWithOptions(context.Background(), inst, url, 4*time.Second, 1)
			_ = clearBalancerOverride(inst, plan.tag)
			if warmErr == nil {
				// The override is the route that actually completed this request.
				// The observatory may still be cold here, so its current principle
				// target is not yet evidence that another route is viable.
				return outboundDelayResult{Delay: delay, OutboundTag: warmTarget}, nil
			}
			log.Printf("warm route %q failed, waiting for fresh observatory result: %v", warmTarget, warmErr)
		} else {
			log.Printf("cached warm route %q is not usable by this policy group: %v", warmTarget, err)
		}
	}

	readinessDeadline := time.Duration(0)
	if len(plan.selectors) > 0 {
		readinessDeadline, err = observationResultDeadline(inst)
		if err != nil {
			return failure, fmt.Errorf("observatory deadline unavailable: %w", err)
		}
	}
	if err := waitForObservatoryReady(inst, plan.selectors, readinessDeadline); err != nil {
		return failure, err
	}
	delay, err := measureInstDelay(context.Background(), inst, url)
	if err != nil {
		return failure, err
	}
	target, _ := firstBalancerPrincipleTarget(inst, plan.tag)
	return outboundDelayResult{Delay: delay, OutboundTag: target}, nil
}

// routedBalancerSelectors returns the selectors used by the first routed
// balancer. Speed-test configurations contain one catch-all balancer rule, so
// these selectors identify the policy-group members that must be observed
// before the measured request is dispatched.
func routedBalancerSelectors(config *core.Config) ([]string, error) {
	plan, err := routedBalancerPlanForConfig(config)
	return plan.selectors, err
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

// waitForObservatoryReady prevents a policy-group measurement from racing the
// temporary core's initial probes. As soon as one candidate is known alive the
// balancer can safely dispatch through it. If every candidate has completed
// and none is alive, fail immediately instead of falling through to a default
// outbound.
func waitForObservatoryReady(inst *core.Instance, selectors []string, timeout time.Duration) error {
	if len(selectors) == 0 {
		return nil
	}

	feature := inst.GetFeature(coreextension.ObservatoryType())
	observer, ok := feature.(coreextension.Observatory)
	if !ok {
		return errors.New("policy-group observatory is unavailable")
	}
	managerFeature := inst.GetFeature(coreoutbound.ManagerType())
	manager, ok := managerFeature.(coreoutbound.HandlerSelector)
	if !ok {
		return errors.New("outbound manager cannot select policy-group members")
	}
	candidates := manager.Select(selectors)
	if len(candidates) == 0 {
		return errors.New("policy group has no candidate outbounds")
	}
	candidateSet := make(map[string]struct{}, len(candidates))
	for _, tag := range candidates {
		candidateSet[tag] = struct{}{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	notifier, ok := feature.(coreextension.ObservatoryUpdateNotifier)
	if !ok {
		return errors.New("policy-group observatory does not publish update notifications")
	}
	updates, unsubscribe := notifier.SubscribeObservationUpdates()
	defer unsubscribe()

	for {
		observation, err := observer.GetObservation(ctx)
		if err != nil {
			return fmt.Errorf("observatory query failed: %w", err)
		}
		result, ok := observation.(*coreobservatory.ObservationResult)
		if !ok {
			return errors.New("unexpected observatory result type")
		}
		ready, complete := policyGroupObservationState(result, candidateSet)
		if ready {
			return nil
		}
		if complete {
			return errors.New("policy group has no healthy outbound")
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("policy-group observatory readiness timeout: %w", ctx.Err())
		case _, open := <-updates:
			if !open {
				return errors.New("policy-group observatory closed before publishing a usable result")
			}
		}
	}
}

func policyGroupObservationState(result *coreobservatory.ObservationResult, candidates map[string]struct{}) (bool, bool) {
	completed := make(map[string]struct{}, len(candidates))
	for _, status := range result.GetStatus() {
		tag := status.GetOutboundTag()
		if _, expected := candidates[tag]; !expected {
			continue
		}
		completed[tag] = struct{}{}
		if status.GetAlive() {
			return true, len(completed) == len(candidates)
		}
	}
	return false, len(completed) == len(candidates)
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

func observationResultDeadline(inst *core.Instance) (time.Duration, error) {
	if inst == nil {
		return 0, errors.New("core instance is nil")
	}
	observer := inst.GetFeature(coreextension.ObservatoryType())
	deadline, ok := observer.(coreextension.ObservatoryProbeDeadline)
	if !ok {
		return 0, errors.New("observatory does not expose its initial probe deadline")
	}
	return observationResultDeadlineForProbe(deadline.ObservationProbeDeadline())
}

func watchBalancerTargetChanges(inst *core.Instance, balancerTag string, handler CoreCallbackHandler) (func(), error) {
	if balancerTag == "" {
		return func() {}, nil
	}
	observer := inst.GetFeature(coreextension.ObservatoryType())
	notifier, ok := observer.(coreextension.ObservatoryUpdateNotifier)
	if !ok {
		return nil, errors.New("observatory does not publish update notifications")
	}

	updates, unsubscribe := notifier.SubscribeObservationUpdates()
	watchCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer unsubscribe()
		lastTarget := ""
		for {
			select {
			case <-watchCtx.Done():
				return
			case _, open := <-updates:
				if !open || watchCtx.Err() != nil {
					return
				}
			}

			target, err := firstBalancerPrincipleTarget(inst, balancerTag)
			if err != nil || target == "" || target == lastTarget {
				continue
			}
			lastTarget = target

			if override, err := getBalancerOverride(inst, balancerTag); err == nil && override != "" {
				if err := clearBalancerOverride(inst, balancerTag); err != nil {
					log.Printf("failed to release warm route %q: %v", override, err)
				} else {
					log.Printf("released warm route %q after fresh observatory result", override)
				}
			}
			if handler != nil {
				handler.OnBalancerTargetChanged(balancerTag, target)
			}
		}
	}()

	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			cancel()
			unsubscribe()
			<-done
		})
	}, nil
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
		warmRouteLifetime, err = observationResultDeadline(instance)
		if err != nil {
			log.Printf("warm route %q was not applied: %v", warmTarget, err)
		} else if err := setBalancerOverride(instance, warmBalancerTag, warmTarget); err != nil {
			log.Printf("warm route %q was not applied: %v", warmTarget, err)
		} else {
			log.Printf("using warm route %q for up to %v while observatory starts", warmTarget, warmRouteLifetime)
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
			if !x.IsRunning || x.coreInstance != instance {
				return
			}
			override, err := getBalancerOverride(instance, warmBalancerTag)
			if err == nil && override == warmTarget {
				_ = clearBalancerOverride(instance, warmBalancerTag)
				log.Printf("warm route %q expired before observatory selected a fresh target", warmTarget)
			}
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
