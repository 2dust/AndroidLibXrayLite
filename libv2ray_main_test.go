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
	corerouter "github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common/serial"
	core "github.com/xtls/xray-core/core"
	coreextension "github.com/xtls/xray-core/features/extension"
	coreoutbound "github.com/xtls/xray-core/features/outbound"
	corerouting "github.com/xtls/xray-core/features/routing"
	coreserial "github.com/xtls/xray-core/infra/conf/serial"
	"google.golang.org/protobuf/proto"
)

type resetTestCallback struct {
	statuses []string
}

func (*resetTestCallback) Startup() int  { return 0 }
func (*resetTestCallback) Shutdown() int { return 0 }
func (c *resetTestCallback) OnEmitStatus(_ int, status string) int {
	c.statuses = append(c.statuses, status)
	return 0
}
func (*resetTestCallback) OnBalancerTargetChanged(string, string) int { return 0 }

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

func TestRoutedBalancerSelectors(t *testing.T) {
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

	selectors, err := routedBalancerSelectors(config)
	if err != nil {
		t.Fatalf("routedBalancerSelectors returned an error: %v", err)
	}
	if want := []string{"proxy-policy-"}; !reflect.DeepEqual(selectors, want) {
		t.Fatalf("selectors = %v, want %v", selectors, want)
	}
	plan, err := routedBalancerPlanForConfig(config)
	if err != nil {
		t.Fatalf("routedBalancerPlanForConfig returned an error: %v", err)
	}
	if plan.tag != "balancer-main" {
		t.Fatalf("balancer tag = %q, want balancer-main", plan.tag)
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

func TestRoutedBalancerSelectorsWithoutObservatory(t *testing.T) {
	routerConfig := &corerouter.Config{
		Rule: []*corerouter.RoutingRule{{
			TargetTag: &corerouter.RoutingRule_BalancingTag{BalancingTag: "balancer-main"},
		}},
	}
	config := &core.Config{App: []*serial.TypedMessage{serial.ToTypedMessage(routerConfig)}}

	selectors, err := routedBalancerSelectors(config)
	if err != nil {
		t.Fatalf("routedBalancerSelectors returned an error: %v", err)
	}
	if selectors != nil {
		t.Fatalf("selectors = %v, want nil", selectors)
	}
}

func TestPolicyGroupObservationState(t *testing.T) {
	candidates := map[string]struct{}{"a": {}, "b": {}}
	tests := []struct {
		name     string
		statuses []*coreobservatory.OutboundStatus
		ready    bool
		complete bool
	}{
		{name: "empty"},
		{
			name:     "partial dead",
			statuses: []*coreobservatory.OutboundStatus{{OutboundTag: "a"}},
		},
		{
			name:     "first healthy",
			statuses: []*coreobservatory.OutboundStatus{{OutboundTag: "a", Alive: true}},
			ready:    true,
		},
		{
			name: "all dead",
			statuses: []*coreobservatory.OutboundStatus{
				{OutboundTag: "a"},
				{OutboundTag: "b"},
			},
			complete: true,
		},
		{
			name: "ignores unrelated status",
			statuses: []*coreobservatory.OutboundStatus{
				{OutboundTag: "unrelated", Alive: true},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ready, complete := policyGroupObservationState(
				&coreobservatory.ObservationResult{Status: test.statuses},
				candidates,
			)
			if ready != test.ready || complete != test.complete {
				t.Fatalf("state = (%v, %v), want (%v, %v)", ready, complete, test.ready, test.complete)
			}
		})
	}
}

type readinessTestObserver struct {
	access     sync.Mutex
	statuses   []*coreobservatory.OutboundStatus
	updates    coreextension.ObservatoryUpdateDispatcher
	subscribed chan struct{}
	once       sync.Once
}

func (*readinessTestObserver) Type() interface{} { return coreextension.ObservatoryType() }
func (*readinessTestObserver) Start() error      { return nil }
func (o *readinessTestObserver) Close() error {
	o.updates.Close()
	return nil
}
func (o *readinessTestObserver) GetObservation(context.Context) (proto.Message, error) {
	o.access.Lock()
	defer o.access.Unlock()
	return &coreobservatory.ObservationResult{Status: append([]*coreobservatory.OutboundStatus(nil), o.statuses...)}, nil
}
func (o *readinessTestObserver) SubscribeObservationUpdates() (<-chan struct{}, func()) {
	updates, unsubscribe := o.updates.SubscribeObservationUpdates()
	o.once.Do(func() { close(o.subscribed) })
	return updates, unsubscribe
}
func (o *readinessTestObserver) publish(statuses ...*coreobservatory.OutboundStatus) {
	o.access.Lock()
	o.statuses = statuses
	o.access.Unlock()
	o.updates.NotifyObservationUpdate()
}

type readinessTestOutboundManager struct {
	tags []string
}

func (*readinessTestOutboundManager) Type() interface{} { return coreoutbound.ManagerType() }
func (*readinessTestOutboundManager) Start() error      { return nil }
func (*readinessTestOutboundManager) Close() error      { return nil }
func (*readinessTestOutboundManager) GetHandler(string) coreoutbound.Handler {
	return nil
}
func (*readinessTestOutboundManager) GetDefaultHandler() coreoutbound.Handler {
	return nil
}
func (*readinessTestOutboundManager) AddHandler(context.Context, coreoutbound.Handler) error {
	return nil
}
func (*readinessTestOutboundManager) RemoveHandler(context.Context, string) error { return nil }
func (*readinessTestOutboundManager) ListHandlers(context.Context) []coreoutbound.Handler {
	return nil
}
func (m *readinessTestOutboundManager) Select([]string) []string {
	return append([]string(nil), m.tags...)
}

func newReadinessTestInstance(t *testing.T) (*core.Instance, *readinessTestObserver) {
	t.Helper()
	observer := &readinessTestObserver{subscribed: make(chan struct{})}
	manager := &readinessTestOutboundManager{tags: []string{"proxy-a", "proxy-b"}}
	instance := &core.Instance{}
	if err := instance.AddFeature(observer); err != nil {
		t.Fatal(err)
	}
	if err := instance.AddFeature(manager); err != nil {
		t.Fatal(err)
	}
	return instance, observer
}

func TestWaitForObservatoryReadyWakesOnUpdate(t *testing.T) {
	instance, observer := newReadinessTestInstance(t)
	done := make(chan error, 1)
	go func() {
		done <- waitForObservatoryReady(instance, []string{"proxy-"}, time.Second)
	}()
	<-observer.subscribed
	observer.publish(&coreobservatory.OutboundStatus{OutboundTag: "proxy-a", Alive: true})

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("observatory update did not wake the readiness wait")
	}
}

func TestWaitForObservatoryReadyReportsClosedUpdates(t *testing.T) {
	instance, observer := newReadinessTestInstance(t)
	done := make(chan error, 1)
	go func() {
		done <- waitForObservatoryReady(instance, []string{"proxy-"}, time.Second)
	}()
	<-observer.subscribed
	observer.updates.Close()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("readiness error = %v, want closed-observatory error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("closed observatory did not end the readiness wait")
	}
}

func TestOutboundDelayResultJSON(t *testing.T) {
	payload, err := json.Marshal(outboundDelayResult{Delay: 42, OutboundTag: "proxy-policy-a"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(payload), `{"delay":42,"outboundTag":"proxy-policy-a"}`; got != want {
		t.Fatalf("payload = %s, want %s", got, want)
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

type outboundProbeTestRouter struct {
	targets map[string][]string
}

func (*outboundProbeTestRouter) Type() interface{} { return corerouting.RouterType() }
func (*outboundProbeTestRouter) Start() error      { return nil }
func (*outboundProbeTestRouter) Close() error      { return nil }
func (r *outboundProbeTestRouter) GetPrincipleTarget(tag string) ([]string, error) {
	return append([]string(nil), r.targets[tag]...), nil
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

func TestOutboundProbeSnapshotPreservesPartialResults(t *testing.T) {
	observer := &readinessTestObserver{subscribed: make(chan struct{})}
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
		context.Canceled,
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
	if result.Completed || !result.Cancelled {
		t.Fatalf("completion state = (%v, %v), want incomplete and cancelled", result.Completed, result.Cancelled)
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
	batch, ok := instance.GetFeature(coreextension.ObservatoryType()).(coreextension.ObservatoryBatchProbe)
	if !ok {
		t.Fatal("composite config did not create a one-shot batch observatory")
	}
	deadline, err := batch.ProbeOutboundsDeadline([]string{"probe-a", "probe-b"}, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if deadline != 12*time.Second {
		t.Fatalf("batch deadline = %v, want 12s", deadline)
	}
}
