package runtime

import (
	"sync"

	"nine/internal/memory"
)

// NewStores returns checkpoint and notification stores backed by store,
// plus a notifAdd function that enqueues a notification for an agent.
func NewStores(store *memory.Store) (
	ckpt CheckpointStore,
	notif NotifStore,
	notifAdd func(agentID, text string),
) {
	log.Debug("NewStores creating stores")
	cs := &SQLCheckpointStore{store: store}
	ns := &SQLNotifStore{store: store}
	return cs, ns, ns.Add
}

// InMemoryCheckpointStore is an in-memory CheckpointStore. Safe for concurrent use.
type InMemoryCheckpointStore struct {
	mu   sync.RWMutex
	data map[string][]byte
}

// NewInMemoryCheckpointStore returns an empty InMemoryCheckpointStore.
func NewInMemoryCheckpointStore() *InMemoryCheckpointStore {
	return &InMemoryCheckpointStore{data: make(map[string][]byte)}
}

func (s *InMemoryCheckpointStore) Save(agentID string, data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	s.mu.Lock()
	s.data[agentID] = cp
	s.mu.Unlock()
	return nil
}

func (s *InMemoryCheckpointStore) Load(agentID string) ([]byte, bool, error) {
	s.mu.RLock()
	d, ok := s.data[agentID]
	s.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	cp := make([]byte, len(d))
	copy(cp, d)
	return cp, true, nil
}

func (s *InMemoryCheckpointStore) Delete(agentID string) error {
	s.mu.Lock()
	delete(s.data, agentID)
	s.mu.Unlock()
	return nil
}

// InMemoryNotifStore is an in-memory NotifStore. Safe for concurrent use.
type InMemoryNotifStore struct {
	mu     sync.RWMutex
	notifs map[string][]string
}

// NewInMemoryNotifStore returns an empty InMemoryNotifStore.
func NewInMemoryNotifStore() *InMemoryNotifStore {
	return &InMemoryNotifStore{notifs: make(map[string][]string)}
}

// Add queues a notification for delivery on the agent's next turn.
func (s *InMemoryNotifStore) Add(agentID, text string) {
	s.mu.Lock()
	s.notifs[agentID] = append(s.notifs[agentID], text)
	s.mu.Unlock()
}

// Fetch returns and clears all pending notifications for the agent.
func (s *InMemoryNotifStore) Fetch(agentID string) ([]string, error) {
	s.mu.Lock()
	ns := s.notifs[agentID]
	delete(s.notifs, agentID)
	s.mu.Unlock()
	return ns, nil
}

// ---- Postgres-backed stores ----

// SQLCheckpointStore persists conversation state in the memory store's
// Postgres conversations table, enabling resume across daemon restarts.
type SQLCheckpointStore struct {
	store *memory.Store
}

func (s *SQLCheckpointStore) Save(agentID string, data []byte) error {
	log.Debug("SQLCheckpointStore.Save", "agentID", agentID, "data_len", len(data))
	return s.store.ConversationSave(agentID, data)
}

func (s *SQLCheckpointStore) Load(agentID string) ([]byte, bool, error) {
	log.Debug("SQLCheckpointStore.Load", "agentID", agentID)
	return s.store.ConversationLoad(agentID)
}

func (s *SQLCheckpointStore) Delete(agentID string) error {
	log.Debug("SQLCheckpointStore.Delete", "agentID", agentID)
	return s.store.ConversationDelete(agentID)
}

// SQLNotifStore persists notifications in the memory store's Postgres
// notifications table.
type SQLNotifStore struct {
	store *memory.Store
}

// Add enqueues a notification for the given agent.
func (s *SQLNotifStore) Add(agentID, text string) {
	log.Debug("SQLNotifStore.Add", "agentID", agentID, "text", text)
	s.store.NotificationCreate(newUUID(), agentID, text, false) //nolint:errcheck
}

func (s *SQLNotifStore) Fetch(agentID string) ([]string, error) {
	log.Debug("SQLNotifStore.Fetch", "agentID", agentID)
	ns, err := s.store.NotificationListPending(agentID)
	if err != nil {
		log.Warn("SQLNotifStore.Fetch NotificationListPending failed", "agentID", agentID, "err", err)
		return nil, err
	}
	msgs := make([]string, 0, len(ns))
	for _, n := range ns {
		msgs = append(msgs, n.Message)
		s.store.NotificationMarkDelivered(n.ID) //nolint:errcheck
	}
	log.Debug("SQLNotifStore.Fetch found notifications", "agentID", agentID, "count", len(msgs))
	return msgs, nil
}
