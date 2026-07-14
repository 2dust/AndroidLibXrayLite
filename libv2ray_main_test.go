package libv2ray

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	coreobservatory "github.com/xtls/xray-core/app/observatory"
	coreobservatoryburst "github.com/xtls/xray-core/app/observatory/burst"
	corerouter "github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common/serial"
	core "github.com/xtls/xray-core/core"
	coreextension "github.com/xtls/xray-core/features/extension"
	corerouting "github.com/xtls/xray-core/features/routing"
	coreserial "github.com/xtls/xray-core/infra/conf/serial"
	"google.golang.org/protobuf/proto"
)

type resetTestCallback struct {
	statuses        []string
	targets         []string
	targetStatus    int
	beforeTargetAck func(string, string)
}

func (*resetTestCallback) Startup() int  { return 0 }
func (*resetTestCallback) Shutdown() int { return 0 }
func (c *resetTestCallback) OnEmitStatus(_ int, status string) int {
	c.statuses = append(c.statuses, status)
	return 0
}
func (c *resetTestCallback) OnBalancerTargetChanged(balancerTag, target string) int {
	if c.beforeTargetAck != nil {
		c.beforeTargetAck(balancerTag, target)
	}
	c.targets = append(c.targets, balancerTag+":"+target)
	return c.targetStatus
}

func TestResetNetworkStateRecoversOriginalConfiguration(t *testing.T) {
	callback := &resetTestCallback{}
	controller := &CoreController{
		CallbackHandler: callback,
		configContent:   "config",
		IsRunning:       true,
	}

	type attempt struct {
		config      string
		balancerTag string
		target      string
	}
	var attempts []attempt
	err := controller.resetNetworkStateWithStarter("balancer", "cached-target", func(config, balancerTag, target string) error {
		attempts = append(attempts, attempt{config, balancerTag, target})
		if len(attempts) == 1 {
			return errors.New("replacement failed")
		}
		controller.IsRunning = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !controller.IsRunning {
		t.Fatal("controller was not running after original configuration recovery")
	}
	wantAttempts := []attempt{
		{config: "config", balancerTag: "balancer", target: "cached-target"},
		{config: "config"},
	}
	if !reflect.DeepEqual(attempts, wantAttempts) {
		t.Fatalf("attempts = %#v, want %#v", attempts, wantAttempts)
	}
	if len(callback.statuses) != 1 || !strings.Contains(callback.statuses[0], "recovered") {
		t.Fatalf("statuses = %q, want one recovery status", callback.statuses)
	}
}

func TestResetNetworkStateReportsFailedRecovery(t *testing.T) {
	callback := &resetTestCallback{}
	controller := &CoreController{
		CallbackHandler: callback,
		configContent:   "config",
		IsRunning:       true,
	}

	attempts := 0
	err := controller.resetNetworkStateWithStarter("balancer", "cached-target", func(string, string, string) error {
		attempts++
		return errors.New("startup failed")
	})
	if err == nil {
		t.Fatal("expected reset and recovery failure")
	}
	if attempts != 2 {
		t.Fatalf("startup attempts = %d, want 2", attempts)
	}
	if controller.IsRunning {
		t.Fatal("controller unexpectedly remained running after both startup attempts failed")
	}
	if !strings.Contains(err.Error(), "original configuration recovery failed") {
		t.Fatalf("error = %q, want recovery failure detail", err)
	}
	if len(callback.statuses) != 1 || !strings.Contains(callback.statuses[0], "recovery failed") {
		t.Fatalf("statuses = %q, want one failed recovery status", callback.statuses)
	}
}

func TestRoutedBalancerPlan(t *testing.T) {
	routerConfig := &corerouter.Config{
		Rule: []*corerouter.RoutingRule{{
			TargetTag: &corerouter.RoutingRule_BalancingTag{BalancingTag: "balancer-main"},
		}},
		BalancingRule: []*corerouter.BalancingRule{{
			Tag:              "balancer-main",
			OutboundSelector: []string{"proxy-policy-"},
			Strategy:         "leastPing",
		}},
	}
	config := &core.Config{App: []*serial.TypedMessage{
		serial.ToTypedMessage(routerConfig),
		serial.ToTypedMessage(&coreobservatory.Config{SubjectSelector: []string{"proxy-policy-"}}),
	}}

	plan, err := routedBalancerPlanForConfig(config)
	if err != nil {
		t.Fatalf("routedBalancerPlanForConfig returned an error: %v", err)
	}
	if plan.tag != "balancer-main" {
		t.Fatalf("balancer tag = %q, want balancer-main", plan.tag)
	}
	if want := []string{"proxy-policy-"}; !reflect.DeepEqual(plan.selectors, want) {
		t.Fatalf("selectors = %v, want %v", plan.selectors, want)
	}
}

func TestRoutedBalancerPlanIgnoresNonObservableStrategy(t *testing.T) {
	routerConfig := &corerouter.Config{
		Rule: []*corerouter.RoutingRule{{
			TargetTag: &corerouter.RoutingRule_BalancingTag{BalancingTag: "balancer-main"},
		}},
		BalancingRule: []*corerouter.BalancingRule{{
			Tag:              "balancer-main",
			OutboundSelector: []string{"proxy-policy-"},
			Strategy:         "random",
		}},
	}
	config := &core.Config{App: []*serial.TypedMessage{
		serial.ToTypedMessage(routerConfig),
		serial.ToTypedMessage(&coreobservatory.Config{SubjectSelector: []string{"proxy-policy-"}}),
	}}

	plan, err := routedBalancerPlanForConfig(config)
	if err != nil {
		t.Fatalf("routedBalancerPlanForConfig returned an error: %v", err)
	}
	if plan.tag != "" || plan.selectors != nil {
		t.Fatalf("plan = %#v, want empty plan", plan)
	}
}

func TestRoutedBalancerPlanWithoutObservatory(t *testing.T) {
	routerConfig := &corerouter.Config{
		Rule: []*corerouter.RoutingRule{{
			TargetTag: &corerouter.RoutingRule_BalancingTag{BalancingTag: "balancer-main"},
		}},
	}
	config := &core.Config{App: []*serial.TypedMessage{serial.ToTypedMessage(routerConfig)}}

	plan, err := routedBalancerPlanForConfig(config)
	if err != nil {
		t.Fatalf("routedBalancerPlanForConfig returned an error: %v", err)
	}
	if plan.tag != "" || plan.selectors != nil {
		t.Fatalf("plan = %#v, want empty plan", plan)
	}
}

func TestObservationResultDeadlineFollowsProbeDeadline(t *testing.T) {
	got, err := observationResultDeadlineForProbe(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if want := 32 * time.Second; got != want {
		t.Fatalf("observation result deadline = %v, want %v", got, want)
	}
	if _, err := observationResultDeadlineForProbe(0); err == nil {
		t.Fatal("expected an error for an invalid probe deadline")
	}
}

func TestObservationResultDeadlineUsesUpstreamBurstConfig(t *testing.T) {
	config := &core.Config{App: []*serial.TypedMessage{serial.ToTypedMessage(
		&coreobservatoryburst.Config{PingConfig: &coreobservatoryburst.HealthPingConfig{
			Timeout: int64(30 * time.Second),
		}},
	)}}
	got, err := observationResultDeadline(config)
	if err != nil {
		t.Fatal(err)
	}
	if want := 32 * time.Second; got != want {
		t.Fatalf("observation result deadline = %v, want %v", got, want)
	}
}

type outboundProbeTestRouter struct {
	targets   map[string][]string
	overrides map[string]string
}

func (*outboundProbeTestRouter) Type() interface{} { return corerouting.RouterType() }
func (*outboundProbeTestRouter) Start() error      { return nil }
func (*outboundProbeTestRouter) Close() error      { return nil }
func (r *outboundProbeTestRouter) GetPrincipleTarget(tag string) ([]string, error) {
	return append([]string(nil), r.targets[tag]...), nil
}
func (r *outboundProbeTestRouter) SetOverrideTarget(tag, target string) error {
	if r.overrides == nil {
		r.overrides = make(map[string]string)
	}
	r.overrides[tag] = target
	return nil
}
func (r *outboundProbeTestRouter) GetOverrideTarget(tag string) (string, error) {
	return r.overrides[tag], nil
}

type outboundProbeTestObserver struct {
	statuses []*coreobservatory.OutboundStatus
}

type outboundProbeTestBurst struct {
	outboundProbeTestObserver
	access sync.Mutex
	checks [][]string
}

func (o *outboundProbeTestBurst) Check(tags []string) {
	o.access.Lock()
	defer o.access.Unlock()
	o.checks = append(o.checks, append([]string(nil), tags...))
}

func (o *outboundProbeTestBurst) checksSnapshot() [][]string {
	o.access.Lock()
	defer o.access.Unlock()
	result := make([][]string, len(o.checks))
	for index, tags := range o.checks {
		result[index] = append([]string(nil), tags...)
	}
	return result
}

type outboundProbeBlockingBurst struct {
	outboundProbeTestObserver
	slowStarted  chan struct{}
	releaseSlow  chan struct{}
	laterStarted chan struct{}
	access       sync.Mutex
	active       int
	maxActive    int
}

func (o *outboundProbeBlockingBurst) Check(tags []string) {
	o.access.Lock()
	o.active++
	if o.active > o.maxActive {
		o.maxActive = o.active
	}
	o.access.Unlock()

	tag := tags[0]
	if tag == "slow" {
		close(o.slowStarted)
		<-o.releaseSlow
	} else if tag == "fast" {
		<-o.slowStarted
	} else if tag == "later" {
		close(o.laterStarted)
	}

	o.access.Lock()
	o.active--
	o.access.Unlock()
}

func (o *outboundProbeBlockingBurst) peakActive() int {
	o.access.Lock()
	defer o.access.Unlock()
	return o.maxActive
}

func (*outboundProbeTestObserver) Type() interface{} { return coreextension.ObservatoryType() }
func (*outboundProbeTestObserver) Start() error      { return nil }
func (*outboundProbeTestObserver) Close() error      { return nil }
func (o *outboundProbeTestObserver) GetObservation(context.Context) (proto.Message, error) {
	return &coreobservatory.ObservationResult{
		Status: append([]*coreobservatory.OutboundStatus(nil), o.statuses...),
	}, nil
}

type outboundProbeTestHandler struct {
	payload string
}

func (h *outboundProbeTestHandler) OnOutboundProbeUpdate(payload string) int {
	h.payload = payload
	return 0
}

func TestOutboundProbeControllerIsSingleUse(t *testing.T) {
	controller := NewOutboundProbeController()
	if _, err := controller.ProbeOutbounds("{}", "not-json", "[]", 1, 1, nil); err == nil {
		t.Fatal("first malformed invocation unexpectedly succeeded")
	}
	if _, err := controller.ProbeOutbounds("{}", "[]", "[]", 1, 1, nil); err == nil ||
		!strings.Contains(err.Error(), "single-use") {
		t.Fatalf("second invocation error = %v, want single-use rejection", err)
	}
}

func TestRunUpstreamBurstChecksPublishesEveryTagAndSample(t *testing.T) {
	observer := &outboundProbeTestBurst{}
	updates := 0
	err := runUpstreamBurstChecks(
		context.Background(),
		observer,
		[]string{"a", "b", "c", "d", "e"},
		2,
		2,
		func() error {
			updates++
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	checks := observer.checksSnapshot()
	counts := make(map[string]int)
	for _, checked := range checks {
		if len(checked) != 1 {
			t.Fatalf("check = %#v, want one tag per upstream call", checked)
		}
		counts[checked[0]]++
	}
	wantCounts := map[string]int{"a": 2, "b": 2, "c": 2, "d": 2, "e": 2}
	if !reflect.DeepEqual(counts, wantCounts) {
		t.Fatalf("check counts = %#v, want %#v", counts, wantCounts)
	}
	if updates != 10 {
		t.Fatalf("snapshot updates = %d, want 10", updates)
	}
}

func TestRunUpstreamBurstProbeGroupsPublishesBeforeSlowPeerFinishes(t *testing.T) {
	observer := &outboundProbeBlockingBurst{
		slowStarted:  make(chan struct{}),
		releaseSlow:  make(chan struct{}),
		laterStarted: make(chan struct{}),
	}
	updates := make(chan struct{}, 3)
	done := make(chan error, 1)
	go func() {
		done <- runUpstreamBurstProbeGroups(
			context.Background(),
			observer,
			[][]string{{"slow", "fast"}, {"later"}},
			1,
			1,
			func() error {
				updates <- struct{}{}
				return nil
			},
		)
	}()

	select {
	case <-observer.slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow probe did not start")
	}
	select {
	case <-updates:
		// The fast result must be visible while the slow peer is still blocked.
	case <-time.After(time.Second):
		t.Fatal("policy-group snapshot waited for the slow peer")
	}
	select {
	case <-observer.laterStarted:
		t.Fatal("second group started before the first group finished")
	default:
	}
	if peak := observer.peakActive(); peak != 2 {
		t.Fatalf("peak active candidate checks = %d, want 2", peak)
	}
	close(observer.releaseSlow)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-observer.laterStarted:
	default:
		t.Fatal("second group did not start after the first group finished")
	}
}

func TestRunUpstreamBurstChecksStopsBetweenChunks(t *testing.T) {
	observer := &outboundProbeTestBurst{}
	ctx, cancel := context.WithCancel(context.Background())
	err := runUpstreamBurstChecks(ctx, observer, []string{"a", "b"}, 1, 1, func() error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if want, got := [][]string{{"a"}}, observer.checksSnapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("checks = %#v, want %#v", got, want)
	}
}

func TestFreshBalancerTargetReleasesWarmOverride(t *testing.T) {
	instance := &core.Instance{}
	if err := instance.AddFeature(&outboundProbeTestObserver{}); err != nil {
		t.Fatal(err)
	}
	router := &outboundProbeTestRouter{
		targets:   map[string][]string{"balancer": {"fresh"}},
		overrides: map[string]string{"balancer": "cached"},
	}
	if err := instance.AddFeature(router); err != nil {
		t.Fatal(err)
	}
	overrideAtCallback := ""
	callback := &resetTestCallback{
		beforeTargetAck: func(balancerTag, _ string) {
			overrideAtCallback, _ = router.GetOverrideTarget(balancerTag)
		},
	}
	lastTarget := handleBalancerTargetChange(instance, "balancer", "", callback)
	if lastTarget != "fresh" {
		t.Fatalf("last target = %q, want fresh", lastTarget)
	}
	if override := router.overrides["balancer"]; override != "" {
		t.Fatalf("warm override = %q, want released", override)
	}
	if overrideAtCallback != "cached" {
		t.Fatalf("override during callback = %q, want cached route retained until acknowledgement", overrideAtCallback)
	}
	if want := []string{"balancer:fresh"}; !reflect.DeepEqual(callback.targets, want) {
		t.Fatalf("target callbacks = %v, want %v", callback.targets, want)
	}
	handleBalancerTargetChange(instance, "balancer", lastTarget, callback)
	if len(callback.targets) != 1 {
		t.Fatalf("duplicate target callback count = %d, want 1", len(callback.targets))
	}
}

func TestWarmRouteHandoffWaitsForCallbackAcknowledgement(t *testing.T) {
	instance := &core.Instance{}
	if err := instance.AddFeature(&outboundProbeTestObserver{}); err != nil {
		t.Fatal(err)
	}
	router := &outboundProbeTestRouter{
		targets:   map[string][]string{"balancer": {"fresh"}},
		overrides: map[string]string{"balancer": "cached"},
	}
	if err := instance.AddFeature(router); err != nil {
		t.Fatal(err)
	}
	callback := &resetTestCallback{targetStatus: 1}
	lastTarget := handleBalancerTargetChange(instance, "balancer", "", callback)
	if lastTarget != "" {
		t.Fatalf("last target = %q, want retry after rejected acknowledgement", lastTarget)
	}
	if override := router.overrides["balancer"]; override != "cached" {
		t.Fatalf("warm override = %q, want retained after rejected acknowledgement", override)
	}

	callback.targetStatus = 0
	lastTarget = handleBalancerTargetChange(instance, "balancer", lastTarget, callback)
	if lastTarget != "fresh" || router.overrides["balancer"] != "" {
		t.Fatalf("successful retry state = (%q, %q), want fresh target and released override", lastTarget, router.overrides["balancer"])
	}
	if want := []string{"balancer:fresh", "balancer:fresh"}; !reflect.DeepEqual(callback.targets, want) {
		t.Fatalf("target callbacks = %v, want %v", callback.targets, want)
	}
}

func TestWarmRouteDeadlineRetainsOverrideWithoutFreshTarget(t *testing.T) {
	instance := &core.Instance{}
	if err := instance.AddFeature(&outboundProbeTestObserver{}); err != nil {
		t.Fatal(err)
	}
	router := &outboundProbeTestRouter{
		targets:   map[string][]string{"balancer": nil},
		overrides: map[string]string{"balancer": "cached"},
	}
	if err := instance.AddFeature(router); err != nil {
		t.Fatal(err)
	}

	reportWarmRouteReadinessDeadline(instance, "balancer", "cached", 7*time.Second)
	if override := router.overrides["balancer"]; override != "cached" {
		t.Fatalf("warm override = %q, want retained until a fresh target exists", override)
	}
	callback := &resetTestCallback{}
	if lastTarget := handleBalancerTargetChange(instance, "balancer", "", callback); lastTarget != "" {
		t.Fatalf("last target = %q, want no target", lastTarget)
	}
	if override := router.overrides["balancer"]; override != "cached" {
		t.Fatalf("warm override after empty observation = %q, want retained", override)
	}
	if len(callback.targets) != 0 {
		t.Fatalf("target callbacks = %v, want none", callback.targets)
	}
}

func TestValidateOutboundProbeConfigRejectsMalformedSource(t *testing.T) {
	if err := ValidateOutboundProbeConfig(`{"outbounds":[{"tag":"proxy","protocol":"freedom"}]}`); err != nil {
		t.Fatalf("valid source was rejected: %v", err)
	}
	if err := ValidateOutboundProbeConfig(`{"outbounds":[{"protocol":"not-a-protocol"}]}`); err == nil {
		t.Fatal("unknown outbound protocol was accepted")
	}
	if err := ValidateOutboundProbeConfig(`{"log":{"loglevel":"warning"}}`); err == nil {
		t.Fatal("source without outbounds was accepted")
	}
}

func TestOutboundProbeSnapshotPreservesPartialResults(t *testing.T) {
	observer := &outboundProbeTestObserver{}
	observer.statuses = []*coreobservatory.OutboundStatus{
		{
			OutboundTag: "probe-b",
			Alive:       false,
			HealthPing: &coreobservatory.HealthPingMeasurementResult{
				All:  1,
				Fail: 1,
			},
		},
		{
			OutboundTag: "probe-a",
			Alive:       true,
			Delay:       42,
			HealthPing: &coreobservatory.HealthPingMeasurementResult{
				All:       1,
				Deviation: 3,
			},
		},
	}
	instance := &core.Instance{}
	if err := instance.AddFeature(observer); err != nil {
		t.Fatal(err)
	}
	if err := instance.AddFeature(&outboundProbeTestRouter{
		targets: map[string][]string{"probe-balancer": {"probe-a"}},
	}); err != nil {
		t.Fatal(err)
	}
	handler := &outboundProbeTestHandler{}
	payload, err := outboundProbeSnapshot(
		instance,
		[]string{"probe-balancer"},
		nil,
		false,
		handler,
	)
	if err != nil {
		t.Fatal(err)
	}
	if handler.payload != payload {
		t.Fatal("progressive callback did not receive the encoded snapshot")
	}
	var result outboundProbeBatchResult
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatal(err)
	}
	if result.Completed || result.Cancelled {
		t.Fatalf("progress state = (%v, %v), want incomplete and active", result.Completed, result.Cancelled)
	}
	if len(result.Results) != 2 || result.Results[0].OutboundTag != "probe-a" {
		t.Fatalf("results = %#v, want two tag-sorted partial results", result.Results)
	}
	if result.Results[0].Delay != 42 || result.Results[0].Samples != 1 || result.Results[0].Deviation != 3 {
		t.Fatalf("probe-a result = %#v", result.Results[0])
	}
	if got := result.BalancerTargets["probe-balancer"]; got != "probe-a" {
		t.Fatalf("balancer target = %q, want probe-a", got)
	}

	finalPayload, err := outboundProbeSnapshot(instance, []string{"probe-balancer"}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if handler.payload != payload {
		t.Fatal("final result was unexpectedly delivered through the progressive callback")
	}
	if err := json.Unmarshal([]byte(finalPayload), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Completed || result.Cancelled {
		t.Fatalf("final state = (%v, %v), want complete and active", result.Completed, result.Cancelled)
	}

	cancelledPayload, err := outboundProbeSnapshot(instance, nil, context.Canceled, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(cancelledPayload), &result); err != nil {
		t.Fatal(err)
	}
	if result.Completed || !result.Cancelled {
		t.Fatalf("cancelled state = (%v, %v), want incomplete and cancelled", result.Completed, result.Cancelled)
	}
}

func TestOneShotProbeConfigContract(t *testing.T) {
	config, err := coreserial.LoadJSONConfig(strings.NewReader(`{
		"log": {"loglevel": "warning"},
		"outbounds": [
			{"tag": "probe-a", "protocol": "freedom"},
			{"tag": "probe-b", "protocol": "freedom"}
		],
		"routing": {
			"domainStrategy": "AsIs",
			"rules": [],
			"balancers": [{
				"tag": "probe-balancer",
				"selector": ["probe-"],
				"strategy": {"type": "leastPing"}
			}]
		},
		"burstObservatory": {
			"subjectSelector": [],
			"pingConfig": {
				"destination": "https://example.com/generate_204",
				"httpMethod": "GET",
				"interval": "1h",
				"sampling": 1,
				"timeout": "12s"
			}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	instance, err := core.New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	_, ok := instance.GetFeature(coreextension.ObservatoryType()).(coreextension.BurstObservatory)
	if !ok {
		t.Fatal("composite config did not create an upstream burst observatory")
	}
}
