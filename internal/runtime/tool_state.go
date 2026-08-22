package runtime

import (
	"errors"

	"nine/internal/memory"
	"nine/internal/toolvm"
)

// toolStateStore adapts the memory store to toolvm.StateStore.
//
// The adapter exists because internal/toolvm has no database and should not grow
// one — the same reason Config.TouchGenerated is a function rather than a store
// handle. It is the only place the two quota types meet, and it translates the
// store's quota refusal into the host's so a guest can tell "you have filled
// your store" apart from "the store is broken".
type toolStateStore struct{ store *memory.Store }

func newToolStateStore(store *memory.Store) toolvm.StateStore {
	if store == nil {
		return nil
	}
	return &toolStateStore{store: store}
}

func (s *toolStateStore) StateGet(tool, scope, key string) (string, bool, error) {
	return s.store.ToolStateGet(tool, scope, key)
}

func (s *toolStateStore) StateSet(tool, scope, key, value string, q toolvm.StateQuota) error {
	return asHostQuotaError(s.store.ToolStateSet(tool, scope, key, value, storeQuota(q)))
}

func (s *toolStateStore) StateDelete(tool, scope, key string) error {
	return s.store.ToolStateDelete(tool, scope, key)
}

func (s *toolStateStore) StateList(tool, scope, prefix string) ([]string, error) {
	return s.store.ToolStateList(tool, scope, prefix)
}

func (s *toolStateStore) StateSwap(tool, scope, key string, expected *string, value string, q toolvm.StateQuota) (bool, error) {
	took, err := s.store.ToolStateSwap(tool, scope, key, expected, value, storeQuota(q))
	return took, asHostQuotaError(err)
}

func storeQuota(q toolvm.StateQuota) memory.ToolStateQuota {
	return memory.ToolStateQuota{
		MaxKeys:    q.MaxKeys,
		MaxValueKB: q.MaxValueKB,
		MaxTotalKB: q.MaxTotalKB,
		TTL:        q.TTL,
	}
}

// asHostQuotaError re-types a store quota refusal as the host's, so the guest
// harness can throw something a tool can distinguish. Any other error passes
// through unchanged — a broken store is not the tool's problem to handle.
func asHostQuotaError(err error) error {
	var qe *memory.ToolStateQuotaError
	if errors.As(err, &qe) {
		return &toolvm.QuotaError{Reason: qe.Reason}
	}
	return err
}
