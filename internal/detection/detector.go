package detection

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/mvaldes14/nerv/internal/investigation"
)

type Acceptor interface {
	Accept(context.Context, investigation.Request) (investigation.Accepted, error)
}

type Clock interface {
	Now() time.Time
}

type Config struct {
	Persistence           time.Duration
	Cooldown              time.Duration
	Resync                time.Duration
	Workers               int
	QueueCapacity         int
	RestartThreshold      int32
	RestartWindow         time.Duration
	Namespaces            []string
	MaintenanceAnnotation string
	AcceptTimeout         time.Duration
}

type Detector struct {
	factory      informers.SharedInformerFactory
	pods         cache.SharedIndexInformer
	deployments  cache.SharedIndexInformer
	statefulSets cache.SharedIndexInformer
	daemonSets   cache.SharedIndexInformer
	acceptor     Acceptor
	tracker      *Tracker
	queue        *boundedQueue
	clock        Clock
	logger       *slog.Logger
	cfg          Config
	namespaces   map[string]struct{}
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func New(client kubernetes.Interface, acceptor Acceptor, cfg Config, logger *slog.Logger) (*Detector, error) {
	if client == nil || acceptor == nil {
		return nil, fmt.Errorf("Kubernetes client and investigation acceptor are required")
	}
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.QueueCapacity < 1 {
		cfg.QueueCapacity = 256
	}
	if cfg.Resync <= 0 {
		cfg.Resync = 30 * time.Second
	}
	if cfg.AcceptTimeout <= 0 {
		cfg.AcceptTimeout = 10 * time.Second
	}
	if cfg.RestartWindow <= 0 {
		cfg.RestartWindow = 10 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	namespaces := make(map[string]struct{}, len(cfg.Namespaces))
	for _, namespace := range cfg.Namespaces {
		if namespace = strings.TrimSpace(namespace); namespace != "" {
			namespaces[namespace] = struct{}{}
		}
	}
	factory := informers.NewSharedInformerFactory(client, cfg.Resync)
	d := &Detector{
		factory:      factory,
		pods:         factory.Core().V1().Pods().Informer(),
		deployments:  factory.Apps().V1().Deployments().Informer(),
		statefulSets: factory.Apps().V1().StatefulSets().Informer(),
		daemonSets:   factory.Apps().V1().DaemonSets().Informer(),
		acceptor:     acceptor,
		tracker:      NewTracker(cfg.Persistence, cfg.Cooldown),
		queue:        newBoundedQueue(cfg.QueueCapacity),
		clock:        realClock{},
		logger:       logger,
		cfg:          cfg,
		namespaces:   namespaces,
	}
	if err := d.registerHandlers(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Detector) registerHandlers() error {
	items := []struct {
		kind     string
		informer cache.SharedIndexInformer
	}{
		{"pod", d.pods},
		{"deployment", d.deployments},
		{"statefulset", d.statefulSets},
		{"daemonset", d.daemonSets},
	}
	for _, item := range items {
		kind := item.kind
		informer := item.informer
		_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj any) { d.enqueue(kind, obj) },
			UpdateFunc: func(_, obj any) { d.enqueue(kind, obj) },
			DeleteFunc: func(obj any) { d.enqueue(kind, obj) },
		})
		if err != nil {
			return fmt.Errorf("register %s informer handler: %w", kind, err)
		}
		if err := informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
			d.logger.Warn("Kubernetes informer watch failed; client-go will retry", "kind", kind, "error", err)
		}); err != nil {
			return fmt.Errorf("register %s watch error handler: %w", kind, err)
		}
	}
	return nil
}

func (d *Detector) Run(ctx context.Context) error {
	var workers sync.WaitGroup
	for range d.cfg.Workers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			d.worker(ctx)
		}()
	}
	defer func() {
		d.queue.Close()
		workers.Wait()
	}()

	d.factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), d.pods.HasSynced, d.deployments.HasSynced, d.statefulSets.HasSynced, d.daemonSets.HasSynced) {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("synchronize Kubernetes informer caches")
	}
	d.enqueueStore("pod", d.pods)
	d.enqueueStore("deployment", d.deployments)
	d.enqueueStore("statefulset", d.statefulSets)
	d.enqueueStore("daemonset", d.daemonSets)

	<-ctx.Done()
	return nil
}

func (d *Detector) enqueue(kind string, obj any) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		d.logger.Warn("discarding Kubernetes informer event", "kind", kind, "error", err)
		return
	}
	if !d.namespaceAllowed(key) {
		return
	}
	if !d.queue.Enqueue(kind + ":" + key) {
		d.logger.Warn("autonomous detection queue full; later informer resync will reconcile state", "kind", kind, "resource", key)
	}
}

func (d *Detector) enqueueStore(kind string, informer cache.SharedIndexInformer) {
	for _, obj := range informer.GetStore().List() {
		d.enqueue(kind, obj)
	}
}

func (d *Detector) worker(ctx context.Context) {
	for {
		key, ok := d.queue.Next(ctx)
		if !ok {
			return
		}
		d.process(ctx, key)
	}
}

func (d *Detector) process(ctx context.Context, key string) {
	kind, namespace, name, ok := splitQueueKey(key)
	if !ok {
		d.logger.Warn("discarding invalid autonomous detection queue key", "key", key)
		return
	}
	resource := kind + ":" + namespace + "/" + name
	var failures []Failure
	var err error
	rules := RuleConfig{
		RestartThreshold:      d.cfg.RestartThreshold,
		RestartWindow:         d.cfg.RestartWindow,
		Now:                   d.clock.Now(),
		MaintenanceAnnotation: d.cfg.MaintenanceAnnotation,
	}
	switch kind {
	case "pod":
		var value *corev1.Pod
		value, err = d.factory.Core().V1().Pods().Lister().Pods(namespace).Get(name)
		if err == nil {
			failures = EvaluatePod(value, rules)
		}
	case "deployment":
		var value *appsv1.Deployment
		value, err = d.factory.Apps().V1().Deployments().Lister().Deployments(namespace).Get(name)
		if err == nil {
			failures = EvaluateDeployment(value, rules)
		}
	case "statefulset":
		var value *appsv1.StatefulSet
		value, err = d.factory.Apps().V1().StatefulSets().Lister().StatefulSets(namespace).Get(name)
		if err == nil {
			failures = EvaluateStatefulSet(value, rules)
		}
	case "daemonset":
		var value *appsv1.DaemonSet
		value, err = d.factory.Apps().V1().DaemonSets().Lister().DaemonSets(namespace).Get(name)
		if err == nil {
			failures = EvaluateDaemonSet(value, rules)
		}
	}
	if err != nil && !apierrors.IsNotFound(err) {
		d.logger.Warn("failed reading informer cache", "kind", kind, "namespace", namespace, "name", name, "error", err)
		return
	}
	for _, failure := range d.tracker.Reconcile(resource, failures, d.clock.Now()) {
		d.trigger(ctx, failure)
	}
}

func (d *Detector) trigger(ctx context.Context, failure Failure) {
	request := investigation.Request{
		IdempotencyKey: investigation.CorrelationKey(&investigation.Target{
			Namespace: failure.Namespace,
			Kind:      failure.Kind,
			Name:      failure.Name,
			Container: failure.Container,
		}, failure.Identity),
		Question: fmt.Sprintf("Investigate autonomous Kubernetes failure: %s. %s", failure.Reason, failure.Summary),
		Source:   "kubernetes.detector",
		Target: &investigation.Target{
			Namespace: failure.Namespace,
			Kind:      failure.Kind,
			Name:      failure.Name,
			Container: failure.Container,
		},
		Labels: map[string]string{"nerv_failure": failure.Identity},
	}
	acceptCtx, cancel := context.WithTimeout(ctx, d.cfg.AcceptTimeout)
	defer cancel()
	accepted, err := d.acceptor.Accept(acceptCtx, request)
	now := d.clock.Now()
	if err != nil {
		d.tracker.Complete(failure, false, now)
		d.logger.Warn("autonomous investigation acceptance failed; next resync will retry", "kind", failure.Kind, "namespace", failure.Namespace, "name", failure.Name, "failure", failure.Identity, "error", err)
		return
	}
	d.tracker.Complete(failure, true, now)
	d.logger.Info("autonomous Kubernetes failure accepted", "incident_id", accepted.ID, "workflow_id", accepted.WorkflowID, "kind", failure.Kind, "namespace", failure.Namespace, "name", failure.Name, "failure", failure.Identity)
}

func (d *Detector) namespaceAllowed(key string) bool {
	if len(d.namespaces) == 0 {
		return true
	}
	slash := strings.IndexByte(key, '/')
	if slash <= 0 {
		return false
	}
	_, ok := d.namespaces[key[:slash]]
	return ok
}

func splitQueueKey(value string) (kind, namespace, name string, ok bool) {
	colon := -1
	for i := range value {
		if value[i] == ':' {
			colon = i
			break
		}
	}
	if colon <= 0 {
		return "", "", "", false
	}
	kind = value[:colon]
	key := value[colon+1:]
	slash := -1
	for i := range key {
		if key[i] == '/' {
			slash = i
			break
		}
	}
	if slash < 0 {
		return kind, "", key, key != ""
	}
	return kind, key[:slash], key[slash+1:], key[slash+1:] != ""
}

type boundedQueue struct {
	items   chan string
	mu      sync.Mutex
	pending map[string]struct{}
	closed  bool
}

func newBoundedQueue(capacity int) *boundedQueue {
	return &boundedQueue{items: make(chan string, capacity), pending: make(map[string]struct{})}
}

func (q *boundedQueue) Enqueue(key string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	if _, ok := q.pending[key]; ok {
		return true
	}
	select {
	case q.items <- key:
		q.pending[key] = struct{}{}
		return true
	default:
		return false
	}
}

func (q *boundedQueue) Next(ctx context.Context) (string, bool) {
	select {
	case <-ctx.Done():
		return "", false
	case key, ok := <-q.items:
		if !ok {
			return "", false
		}
		q.mu.Lock()
		delete(q.pending, key)
		q.mu.Unlock()
		return key, true
	}
}

func (q *boundedQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.items)
}
