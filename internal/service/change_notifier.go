package service

import "sync"

// Change is a commit this server made for an instance of a project.
type Change struct {
	Instance string
	// Type is ADDED, MODIFIED or DELETED.
	Type string
}

// ChangeNotifier tells the live streams about the server's own commits, which
// the cluster only reflects once the engine reconciles them. A slow listener
// misses changes rather than blocking a write.
type ChangeNotifier struct {
	mu   sync.Mutex
	subs map[string]map[chan Change]struct{}
}

func NewChangeNotifier() *ChangeNotifier {
	return &ChangeNotifier{subs: map[string]map[chan Change]struct{}{}}
}

// Subscribe returns the changes of a project and the function ending the subscription.
func (n *ChangeNotifier) Subscribe(project string) (<-chan Change, func()) {
	ch := make(chan Change, 16)
	n.mu.Lock()
	if n.subs[project] == nil {
		n.subs[project] = map[chan Change]struct{}{}
	}
	n.subs[project][ch] = struct{}{}
	n.mu.Unlock()
	return ch, func() {
		n.mu.Lock()
		delete(n.subs[project], ch)
		n.mu.Unlock()
	}
}

// Notify publishes a change to the subscribers of its project.
func (n *ChangeNotifier) Notify(project, instance, changeType string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for ch := range n.subs[project] {
		select {
		case ch <- Change{Instance: instance, Type: changeType}:
		default:
		}
	}
}
