package agent

import (
	"reflect"
	"strings"
	"testing"
)

// observerFields returns the names of the Loop's callback fields that are
// currently non-nil. It reads unexported fields reflectively, which is why this
// test lives in package agent rather than agent_test.
func observerFields(l *Loop) []string {
	v := reflect.ValueOf(l).Elem()
	t := v.Type()
	var set []string
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Type.Kind() != reflect.Func || !strings.HasPrefix(f.Name, "on") {
			continue
		}
		if !v.Field(i).IsNil() {
			set = append(set, f.Name)
		}
	}
	return set
}

// nonNil builds a do-nothing function of the given func type.
func nonNil(t reflect.Type) reflect.Value {
	return reflect.MakeFunc(t, func([]reflect.Value) []reflect.Value { return nil })
}

// Every exported field of Hooks must reach exactly one Loop callback.
//
// SetHooks assigns the fields by hand, so adding a field to Hooks and forgetting
// the assignment would leave a callback that is silently never invoked — the
// exact failure the Hooks struct replaced twelve separate setters to remove.
// Setting one field at a time and counting what got wired catches both a missed
// assignment (0) and a copy-paste that wires the wrong one twice (2).
func TestSetHooksWiresEveryField(t *testing.T) {
	ht := reflect.TypeOf(Hooks{})
	for i := range ht.NumField() {
		name := ht.Field(i).Name

		var h Hooks
		hv := reflect.ValueOf(&h).Elem()
		hv.Field(i).Set(nonNil(hv.Field(i).Type()))

		l := &Loop{}
		l.SetHooks(h)

		switch got := observerFields(l); len(got) {
		case 1:
			// Exactly one callback wired, as intended.
		case 0:
			t.Errorf("Hooks.%s is never assigned by SetHooks; it would silently never fire", name)
		default:
			t.Errorf("Hooks.%s wired %d callbacks (%v), want exactly 1", name, len(got), got)
		}
	}
}

// The Loop must not carry a callback that Hooks cannot reach: one would be
// permanently nil now that the individual setters are gone.
func TestEveryLoopObserverIsReachableFromHooks(t *testing.T) {
	var all Hooks
	hv := reflect.ValueOf(&all).Elem()
	for i := range hv.NumField() {
		hv.Field(i).Set(nonNil(hv.Field(i).Type()))
	}

	l := &Loop{}
	l.SetHooks(all)

	lt := reflect.TypeOf(l).Elem()
	wired := map[string]bool{}
	for _, n := range observerFields(l) {
		wired[n] = true
	}
	for i := range lt.NumField() {
		f := lt.Field(i)
		if f.Type.Kind() != reflect.Func || !strings.HasPrefix(f.Name, "on") {
			continue
		}
		if !wired[f.Name] {
			t.Errorf("Loop.%s has no corresponding Hooks field; it can never be set", f.Name)
		}
	}
}

// ClearHooks is all-or-nothing. Detaching used to be one nil assignment per
// callback, which is how a set could be left half-attached across turns.
func TestClearHooksDetachesEverything(t *testing.T) {
	var all Hooks
	hv := reflect.ValueOf(&all).Elem()
	for i := range hv.NumField() {
		hv.Field(i).Set(nonNil(hv.Field(i).Type()))
	}

	l := &Loop{}
	l.SetHooks(all)
	if got := observerFields(l); len(got) != hv.NumField() {
		t.Fatalf("SetHooks wired %d callbacks, want %d", len(got), hv.NumField())
	}

	l.ClearHooks()
	if got := observerFields(l); len(got) != 0 {
		t.Errorf("ClearHooks left %v attached", got)
	}
}

// SetHooks replaces the previous set wholesale rather than merging into it, so a
// caller cannot accumulate callbacks from an earlier turn.
func TestSetHooksReplacesRatherThanMerges(t *testing.T) {
	l := &Loop{}
	l.SetHooks(Hooks{OnNotice: func(string) {}, OnPlanStart: func() {}})
	if got := observerFields(l); len(got) != 2 {
		t.Fatalf("first SetHooks wired %v, want 2 callbacks", got)
	}

	l.SetHooks(Hooks{OnNotice: func(string) {}})
	got := observerFields(l)
	if len(got) != 1 || got[0] != "onNotice" {
		t.Errorf("second SetHooks left %v; want only onNotice, with onPlanStart dropped", got)
	}
}
