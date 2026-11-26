// Package fake is an in-memory deployment engine that behaves like a git-backed
// one: a write is accepted with a commit-like revision, the instance stays
// Pending until the test advances it, then becomes Ready. It exists to check
// that the core and the console hold up against an asynchronous engine.
package fake

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// watchBuffer bounds the events queued for a slow watcher. A watcher that
// falls further behind has its channel closed and must watch again.
const watchBuffer = 64

type record struct {
	desired   engine.Instance
	revision  string
	published string
	applied   string
	phase     engine.Phase
	deleting  bool
	createdAt time.Time
}

// watcher is one open watch. done closes with events, so the goroutine waiting
// for the watch context to end can leave as well.
type watcher struct {
	events chan engine.InstanceEvent
	done   chan struct{}
}

type idempotent struct {
	fingerprint string
	operation   engine.Operation
}

// Engine is the fake engine. Its zero value is not usable, use New.
type Engine struct {
	mu          sync.Mutex
	commits     int
	instances   map[engine.InstanceRef]*record
	catalog     []models.PlatformService
	watchers    map[string][]*watcher
	idempotency map[string]idempotent
	next        *engine.Operation
	now         func() time.Time
	buildStep   bool
	hideViews   bool
}

// Option tunes a fake engine.
type Option func(*Engine)

// WithBuildStep makes the engine build each desired revision into a published
// one, as a CI compiling a git source would, before applying it.
func WithBuildStep() Option {
	return func(e *Engine) { e.buildStep = true }
}

// WithoutWriteViews makes writes answer without describing the instance, as an
// engine that only learns the result later would.
func WithoutWriteViews() Option {
	return func(e *Engine) { e.hideViews = true }
}

// New returns an empty fake engine.
func New(options ...Option) *Engine {
	e := &Engine{
		instances:   map[engine.InstanceRef]*record{},
		watchers:    map[string][]*watcher{},
		idempotency: map[string]idempotent{},
		now:         time.Now,
	}
	for _, option := range options {
		option(e)
	}
	return e
}

// Engine returns the four ports, all served by e.
func (e *Engine) Engine() engine.Engine {
	return engine.Engine{
		Info: engine.Info{
			Name: "fake",
			Persistence: engine.Persistence{
				Mode: engine.PersistenceGit,
			},
		},
		DesiredState:   e,
		Observer:       e,
		Catalog:        e,
		PlatformConfig: e,
	}
}

// RejectNext makes the next instance write rejected with problem, storing nothing.
func (e *Engine) RejectNext(problem engine.Problem) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.next = &engine.Operation{Outcome: engine.OutcomeRejected, Problems: []engine.Problem{problem}}
}

// UnknownNext makes the next instance write answer OutcomeUnknown. The write
// is stored anyway, as a push whose answer was lost would be.
func (e *Engine) UnknownNext() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.next = &engine.Operation{Outcome: engine.OutcomeUnknown}
}

// Advance moves an instance one step: Pending to Installing or Updating, then
// to Ready. A deleting instance disappears.
func (e *Engine) Advance(ref engine.InstanceRef) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.advance(ref)
}

// Settle advances an instance until nothing is left to do.
func (e *Engine) Settle(ref engine.InstanceRef) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := 0; i < 3; i++ {
		e.advance(ref)
	}
}

func (e *Engine) advance(ref engine.InstanceRef) {
	r, ok := e.instances[ref]
	if !ok {
		return
	}
	if r.deleting {
		delete(e.instances, ref)
		e.publish(engine.EventDeleted, e.view(ref, r))
		return
	}
	switch r.phase {
	case engine.PhasePending:
		if r.applied == "" {
			r.phase = engine.PhaseInstalling
		} else {
			r.phase = engine.PhaseUpdating
		}
		if e.buildStep {
			sum := sha1.Sum([]byte("build-" + r.revision))
			r.published = hex.EncodeToString(sum[:])
		}
	case engine.PhaseInstalling, engine.PhaseUpdating:
		r.phase = engine.PhaseReady
		r.applied = r.revision
	default:
		return
	}
	e.publish(engine.EventModified, e.view(ref, r))
}

func (e *Engine) commit() string {
	e.commits++
	sum := sha1.Sum([]byte("commit-" + strconv.Itoa(e.commits)))
	return hex.EncodeToString(sum[:])
}

// takeNext returns the forced outcome of this write, if any.
func (e *Engine) takeNext() *engine.Operation {
	next := e.next
	e.next = nil
	return next
}

func fingerprint(in engine.Instance) string {
	raw, _ := json.Marshal(in)
	return string(raw)
}

func (e *Engine) CreateInstance(_ context.Context, in engine.Instance, opts engine.WriteOptions) (engine.Operation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if opts.IdempotencyKey != "" {
		if previous, ok := e.idempotency[opts.IdempotencyKey]; ok {
			if previous.fingerprint != fingerprint(in) {
				return engine.Rejected(engine.CodeConflict, "idempotency key reused for a different request"), nil
			}
			return copyOperation(previous.operation), nil
		}
	}
	forced := e.takeNext()
	if forced != nil && forced.Outcome == engine.OutcomeRejected {
		return *forced, nil
	}
	if _, exists := e.instances[in.Ref]; exists {
		return engine.Rejected(engine.CodeInstanceExists,
			fmt.Sprintf("instance %q already exists in project %q", in.Ref.Name, in.Ref.Project)), nil
	}

	r := &record{desired: copyInstance(in), revision: e.commit(), phase: engine.PhasePending, createdAt: e.now().UTC()}
	e.instances[in.Ref] = r
	view := e.view(in.Ref, r)
	e.publish(engine.EventAdded, view)

	op := engine.Operation{Outcome: engine.OutcomeAccepted, DesiredRevision: r.revision, Instance: e.answerView(view)}
	if forced != nil {
		op = engine.Operation{Outcome: engine.OutcomeUnknown}
	}
	if opts.IdempotencyKey != "" && op.Outcome == engine.OutcomeAccepted {
		e.idempotency[opts.IdempotencyKey] = idempotent{fingerprint: fingerprint(in), operation: copyOperation(op)}
	}
	return op, nil
}

func (e *Engine) UpdateInstance(_ context.Context, in engine.Instance, opts engine.WriteOptions) (engine.Operation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	r, ok := e.instances[in.Ref]
	if !ok {
		return engine.Operation{}, notFound(in.Ref)
	}
	forced := e.takeNext()
	if forced != nil && forced.Outcome == engine.OutcomeRejected {
		return *forced, nil
	}
	if opts.ExpectedRevision != "" && opts.ExpectedRevision != r.revision {
		return engine.Rejected(engine.CodeConflict,
			fmt.Sprintf("instance %q changed since revision %s", in.Ref.Name, opts.ExpectedRevision)), nil
	}

	r.desired = copyInstance(in)
	r.revision = e.commit()
	r.phase = engine.PhasePending
	view := e.view(in.Ref, r)
	e.publish(engine.EventModified, view)

	if forced != nil {
		return engine.Operation{Outcome: engine.OutcomeUnknown}, nil
	}
	return engine.Operation{Outcome: engine.OutcomeAccepted, DesiredRevision: r.revision, Instance: e.answerView(view)}, nil
}

func (e *Engine) DeleteInstance(_ context.Context, ref engine.InstanceRef, opts engine.WriteOptions) (engine.Operation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	r, ok := e.instances[ref]
	if !ok {
		return engine.Operation{}, notFound(ref)
	}
	forced := e.takeNext()
	if forced != nil && forced.Outcome == engine.OutcomeRejected {
		return *forced, nil
	}
	if opts.ExpectedRevision != "" && opts.ExpectedRevision != r.revision {
		return engine.Rejected(engine.CodeConflict,
			fmt.Sprintf("instance %q changed since revision %s", ref.Name, opts.ExpectedRevision)), nil
	}

	r.deleting = true
	r.revision = e.commit()
	view := e.view(ref, r)
	e.publish(engine.EventModified, view)

	if forced != nil {
		return engine.Operation{Outcome: engine.OutcomeUnknown}, nil
	}
	return engine.Operation{Outcome: engine.OutcomeAccepted, DesiredRevision: r.revision, Instance: e.answerView(view)}, nil
}

func (e *Engine) GetInstance(_ context.Context, ref engine.InstanceRef) (engine.Instance, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	r, ok := e.instances[ref]
	if !ok {
		return engine.Instance{}, "", notFound(ref)
	}
	return copyInstance(r.desired), r.revision, nil
}

func (e *Engine) ListInstances(_ context.Context, project string) ([]engine.Instance, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	var out []engine.Instance
	for _, ref := range e.refs(project) {
		out = append(out, copyInstance(e.instances[ref].desired))
	}
	return out, nil
}

func (e *Engine) ObserveInstance(_ context.Context, ref engine.InstanceRef) (engine.InstanceView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	r, ok := e.instances[ref]
	if !ok {
		return engine.InstanceView{}, notFound(ref)
	}
	return e.view(ref, r), nil
}

func (e *Engine) ObserveInstances(_ context.Context, project string) ([]engine.InstanceView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	var out []engine.InstanceView
	for _, ref := range e.refs(project) {
		out = append(out, e.view(ref, e.instances[ref]))
	}
	return out, nil
}

func (e *Engine) EngineName(ref engine.InstanceRef) string {
	return engineName(ref)
}

func (e *Engine) Workloads(_ context.Context, namespace, name string) (engine.Workloads, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, ref := range e.refs(namespace) {
		if engineName(ref) == name {
			return engine.Workloads{Names: []string{name}}, nil
		}
	}
	return engine.Workloads{}, fmt.Errorf("instance %q in project %q: %w", name, namespace, engine.ErrNotFound)
}

func (e *Engine) WatchInstances(ctx context.Context, project string) (<-chan engine.InstanceEvent, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	w := &watcher{events: make(chan engine.InstanceEvent, watchBuffer), done: make(chan struct{})}
	e.watchers[project] = append(e.watchers[project], w)
	go func() {
		select {
		case <-ctx.Done():
			e.mu.Lock()
			defer e.mu.Unlock()
			e.dropWatcher(project, w)
		case <-w.done:
		}
	}()
	return w.events, nil
}

// publish hands each of the project's watchers its own copy of an event,
// closing those that cannot keep up. Called with e.mu held.
func (e *Engine) publish(kind engine.EventType, view engine.InstanceView) {
	for _, w := range append([]*watcher(nil), e.watchers[view.Ref.Project]...) {
		select {
		case w.events <- engine.InstanceEvent{Type: kind, Instance: copyView(view)}:
		default:
			e.dropWatcher(view.Ref.Project, w)
		}
	}
}

// dropWatcher closes a watcher once. Called with e.mu held.
func (e *Engine) dropWatcher(project string, target *watcher) {
	watchers := e.watchers[project]
	for i, w := range watchers {
		if w == target {
			e.watchers[project] = append(watchers[:i], watchers[i+1:]...)
			close(w.events)
			close(w.done)
			return
		}
	}
}

// WatcherCount reports the open watches of a project.
func (e *Engine) WatcherCount(project string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.watchers[project])
}

func (e *Engine) refs(project string) []engine.InstanceRef {
	var refs []engine.InstanceRef
	for ref := range e.instances {
		if ref.Project == project {
			refs = append(refs, ref)
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs
}

func (e *Engine) view(ref engine.InstanceRef, r *record) engine.InstanceView {
	in := copyInstance(r.desired)
	return engine.InstanceView{
		Ref:             ref,
		EngineName:      engineName(ref),
		Service:         in.Service,
		Version:         in.Version,
		Parameters:      in.Parameters,
		TargetNamespace: ref.Project,
		CreatedAt:       r.createdAt,
		Status: engine.InstanceStatus{
			Phase:             r.phase,
			EnginePhase:       string(r.phase),
			Deleting:          r.deleting,
			DesiredRevision:   r.revision,
			PublishedRevision: r.published,
			AppliedRevision:   r.applied,
		},
	}
}

// answerView is the view a write answers with, nil when the engine hides them.
func (e *Engine) answerView(view engine.InstanceView) *engine.InstanceView {
	if e.hideViews {
		return nil
	}
	return &view
}

func engineName(ref engine.InstanceRef) string {
	return ref.Project + "-" + ref.Name
}

func notFound(ref engine.InstanceRef) error {
	return fmt.Errorf("instance %q in project %q: %w", ref.Name, ref.Project, engine.ErrNotFound)
}

func copyInstance(in engine.Instance) engine.Instance {
	out := in
	out.Parameters = copyParameters(in.Parameters)
	return out
}

func copyParameters(parameters map[string]any) map[string]any {
	if parameters == nil {
		return nil
	}
	raw, _ := json.Marshal(parameters)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

func copyView(view engine.InstanceView) engine.InstanceView {
	out := view
	out.Parameters = copyParameters(view.Parameters)
	return out
}

func copyOperation(op engine.Operation) engine.Operation {
	out := op
	if op.Instance != nil {
		view := copyView(*op.Instance)
		out.Instance = &view
	}
	return out
}

// --- Catalog entries ---

func (e *Engine) CreateCatalogEntry(_ context.Context, svc models.PlatformService, _ engine.WriteOptions) (engine.Operation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.catalog {
		if e.catalog[i].Name == svc.Name {
			return engine.Rejected(engine.CodeConflict, fmt.Sprintf("catalog entry %q already exists", svc.Name)), nil
		}
	}
	e.catalog = append(e.catalog, svc)
	return engine.Operation{Outcome: engine.OutcomeAccepted, DesiredRevision: e.commit()}, nil
}

func (e *Engine) UpdateCatalogEntry(_ context.Context, svc models.PlatformService, _ engine.WriteOptions) (engine.Operation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.catalog {
		if e.catalog[i].Name == svc.Name {
			e.catalog[i] = svc
			return engine.Operation{Outcome: engine.OutcomeAccepted, DesiredRevision: e.commit()}, nil
		}
	}
	return engine.Operation{}, fmt.Errorf("catalog entry %q: %w", svc.Name, engine.ErrNotFound)
}

func (e *Engine) DeleteCatalogEntry(_ context.Context, name string, _ engine.WriteOptions) (engine.Operation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.catalog {
		if e.catalog[i].Name == name {
			e.catalog = append(e.catalog[:i], e.catalog[i+1:]...)
			return engine.Operation{Outcome: engine.OutcomeAccepted, DesiredRevision: e.commit()}, nil
		}
	}
	return engine.Operation{}, fmt.Errorf("catalog entry %q: %w", name, engine.ErrNotFound)
}

// --- Catalog ---

func (e *Engine) Entries(ctx context.Context) ([]models.PlatformService, error) {
	return e.Services(ctx)
}

func (e *Engine) Services(context.Context) ([]models.PlatformService, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]models.PlatformService(nil), e.catalog...), nil
}

func (e *Engine) Categories(context.Context) ([]models.MenuCategory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []models.MenuCategory
	seen := map[string]bool{}
	for _, svc := range e.catalog {
		if svc.Category == "" || seen[svc.Category] {
			continue
		}
		seen[svc.Category] = true
		out = append(out, models.MenuCategory{Key: svc.Category, Label: svc.Category, Order: len(out)})
	}
	return out, nil
}

func (e *Engine) Versions(_ context.Context, service string) (*models.ServiceVersions, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, svc := range e.catalog {
		if svc.Name == service {
			return &models.ServiceVersions{Versions: append([]string(nil), svc.Versions...), Default: svc.DefaultVersion}, nil
		}
	}
	return nil, fmt.Errorf("catalog entry %q: %w", service, engine.ErrNotFound)
}

// ParameterSchema accepts any object.
func (e *Engine) ParameterSchema(context.Context, string, string) (map[string]any, error) {
	return map[string]any{"type": "object"}, nil
}

func (e *Engine) Inputs(context.Context, string, string) ([]models.PackageInput, error) {
	return nil, nil
}

// --- Platform configuration ---

func (e *Engine) IngressSuffix(context.Context) (string, error) {
	return "fake.local", nil
}

func (e *Engine) ProfileImages(context.Context) (map[string][]models.ProfileImage, error) {
	return map[string][]models.ProfileImage{}, nil
}

func (e *Engine) SparkConfig(context.Context) (*models.SparkConfig, error) {
	return &models.SparkConfig{}, nil
}
