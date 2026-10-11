package sqldb

import (
	"database/sql/driver"
	"fmt"
	"reflect"
	"sync"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// The authorizer shim. modernc.org/sqlite binds sqlite3_set_authorizer but
// exposes no Go API for it, so nine registers one the way the driver registers
// its own hooks (vendor/modernc.org/sqlite/hooks.go): a trampoline with the C
// signature, passed by function pointer, finding its Go state in a registry
// keyed by the connection handle. The handle and the connection's TLS are read
// from the driver's unexported conn fields by reflection; TestAuthorizerShim
// fails if a driver upgrade moves them.

// authorizer decides one action: ok, or a denial with its reason.
type authorizer func(action int32, arg1, arg2, db, trigger string) bool

// authState is what the trampoline consults for one connection while a
// statement is being prepared.
type authState struct {
	policy authorizer
	// denied names the first action refused, for the error.
	denied string
	// writes are the writes the statement makes, for the journal summary.
	writes []Write
}

var authRegistry = struct {
	mu sync.RWMutex
	m  map[uintptr]*authState
}{m: map[uintptr]*authState{}}

// rawConn is the driver's connection handle and TLS.
type rawConn struct {
	db  uintptr
	tls *libc.TLS
}

// rawOf reads the handle and TLS of a modernc driver connection.
func rawOf(dc driver.Conn) (rawConn, error) {
	v := reflect.ValueOf(dc)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return rawConn{}, fmt.Errorf("sqldb: unexpected driver connection %T", dc)
	}
	db, tls := v.Elem().FieldByName("db"), v.Elem().FieldByName("tls")
	if !db.IsValid() || db.Kind() != reflect.Uintptr || !tls.IsValid() || tls.Type() != reflect.TypeOf((*libc.TLS)(nil)) {
		return rawConn{}, fmt.Errorf("sqldb: the sqlite driver's connection no longer has the db and tls fields the authorizer needs")
	}
	// Audited: tls is the driver's *libc.TLS, type-checked just above; the
	// pointer is read from it, not made up.
	return rawConn{db: uintptr(db.Uint()), tls: (*libc.TLS)(unsafe.Pointer(tls.Pointer()))}, nil //nolint:gosec // G103, audited above
}

// install sets st as the connection's authorizer until the returned func runs.
func (c rawConn) install(st *authState) (uninstall func()) {
	authRegistry.mu.Lock()
	authRegistry.m[c.db] = st
	authRegistry.mu.Unlock()
	sqlite3.Xsqlite3_set_authorizer(c.tls, c.db, cFuncPointer(authTrampoline), c.db)
	return func() {
		sqlite3.Xsqlite3_set_authorizer(c.tls, c.db, 0, 0)
		authRegistry.mu.Lock()
		delete(authRegistry.m, c.db)
		authRegistry.mu.Unlock()
	}
}

// authTrampoline is the C callback: int xAuth(void*, int, const char*,
// const char*, const char*, const char*).
func authTrampoline(tls *libc.TLS, pArg uintptr, action int32, z1, z2, z3, z4 uintptr) int32 {
	authRegistry.mu.RLock()
	st := authRegistry.m[pArg]
	authRegistry.mu.RUnlock()
	if st == nil {
		return sqlite3.SQLITE_DENY
	}
	a1, a2, db, trig := cstr(z1), cstr(z2), cstr(z3), cstr(z4)
	if !st.policy(action, a1, a2, db, trig) {
		if st.denied == "" {
			st.denied = describe(action, a1, a2, db)
		}
		return sqlite3.SQLITE_DENY
	}
	if w, ok := writeOf(action, a1, a2); ok && trig == "" {
		st.writes = append(st.writes, w)
	}
	return sqlite3.SQLITE_OK
}

func cstr(p uintptr) string {
	if p == 0 {
		return ""
	}
	return libc.GoString(p)
}

// cFuncPointer is the driver's own conversion of a Go function to a C function
// pointer (vendor/modernc.org/sqlite/sqlite.go), which it does not export.
func cFuncPointer[T any](f T) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct{ f T }{f})) //nolint:gosec // G103: the driver's own conversion, copied verbatim
}
