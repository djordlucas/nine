package sqldb

import (
	"strings"

	sqlite3 "modernc.org/sqlite/lib"
)

// The policies are allowlists, ported from nine-will's src/kernel/sql-policy.ts:
// an action not named is denied. The database is Nine's alone, so where will
// confined a mind to its being_* tables, these confine a process to tables that
// are not SQLite's own, in the main database.

// schemaTables appear in every DDL statement, which SQLite carries out by
// writing them; they may be read for introspection. A direct write needs
// PRAGMA writable_schema, which no policy allows.
var schemaTables = map[string]bool{"sqlite_master": true, "sqlite_schema": true, "sqlite_sequence": true}

// introspection are the read-only pragmas a statement may run.
var introspection = map[string]bool{
	"table_info": true, "table_xinfo": true, "table_list": true, "index_list": true,
	"index_info": true, "index_xinfo": true, "foreign_key_list": true,
}

// vtableModules are the virtual table modules a process may create.
var vtableModules = map[string]bool{"fts5": true}

// denyFunctions are functions no statement may call.
var denyFunctions = map[string]bool{"load_extension": true}

func isMain(db string) bool { return db == "" || db == "main" }

// isOwn reports whether a table is Nine's rather than SQLite's.
func isOwn(name string) bool {
	return name != "" && !strings.HasPrefix(strings.ToLower(name), "sqlite_")
}

// readPolicy is sql_query's: reads and introspection, nothing else.
func readPolicy(action int32, a1, a2, db, _ string) bool {
	switch action {
	case sqlite3.SQLITE_SELECT, sqlite3.SQLITE_RECURSIVE:
		return true
	case sqlite3.SQLITE_FUNCTION:
		return !denyFunctions[strings.ToLower(a2)]
	case sqlite3.SQLITE_READ:
		return isMain(db)
	case sqlite3.SQLITE_PRAGMA:
		return pragmaOK(a1, a2)
	}
	return false
}

// writePolicy is a program's: reads, writes and DDL on Nine's tables in the
// main database. No ATTACH, no transactions or savepoints (statements run one
// by one on a shared connection), no temp objects, no pragmas beyond
// introspection.
func writePolicy(action int32, a1, a2, db, trig string) bool {
	switch action {
	case sqlite3.SQLITE_SELECT, sqlite3.SQLITE_RECURSIVE, sqlite3.SQLITE_FUNCTION, sqlite3.SQLITE_READ, sqlite3.SQLITE_PRAGMA:
		return readPolicy(action, a1, a2, db, trig)
	case sqlite3.SQLITE_INSERT, sqlite3.SQLITE_UPDATE, sqlite3.SQLITE_DELETE:
		return isMain(db) && (isOwn(a1) || schemaTables[strings.ToLower(a1)])
	case sqlite3.SQLITE_CREATE_TABLE:
		// SQLite creates sqlite_sequence itself for the first AUTOINCREMENT table.
		return isMain(db) && (isOwn(a1) || strings.EqualFold(a1, "sqlite_sequence"))
	case sqlite3.SQLITE_DROP_TABLE, sqlite3.SQLITE_CREATE_VIEW, sqlite3.SQLITE_DROP_VIEW:
		return isMain(db) && isOwn(a1)
	case sqlite3.SQLITE_CREATE_INDEX, sqlite3.SQLITE_DROP_INDEX, sqlite3.SQLITE_CREATE_TRIGGER, sqlite3.SQLITE_DROP_TRIGGER:
		// a1: the index or trigger, a2: its table.
		return isMain(db) && isOwn(a2)
	case sqlite3.SQLITE_CREATE_VTABLE, sqlite3.SQLITE_DROP_VTABLE:
		return isMain(db) && isOwn(a1) && vtableModules[strings.ToLower(a2)]
	case sqlite3.SQLITE_REINDEX:
		// Fired by CREATE INDEX with the new index's name, an autoindex's included.
		return isMain(db) && (isOwn(a1) || strings.HasPrefix(strings.ToLower(a1), "sqlite_autoindex_"))
	case sqlite3.SQLITE_ALTER_TABLE:
		// a1: the database, a2: the table.
		return isMain(a1) && isOwn(a2)
	case sqlite3.SQLITE_ANALYZE:
		return isMain(db)
	}
	return false
}

// pragmaOK allows introspection of a table, and data_version, which FTS5 reads
// on every write.
func pragmaOK(name, arg string) bool {
	name = strings.ToLower(name)
	if name == "data_version" && arg == "" {
		return true
	}
	return introspection[name]
}

// Write is one write a statement makes: its kind and table, never values.
type Write struct {
	Kind  string `json:"kind"`
	Table string `json:"table"`
}

// writeOf names the write an allowed action makes, if it is one. Writes SQLite
// makes to its schema tables while carrying out DDL are left out: the DDL is.
func writeOf(action int32, a1, a2 string) (Write, bool) {
	switch action {
	case sqlite3.SQLITE_INSERT, sqlite3.SQLITE_UPDATE, sqlite3.SQLITE_DELETE:
		if schemaTables[strings.ToLower(a1)] {
			return Write{}, false
		}
		return Write{Kind: actions[action], Table: a1}, true
	case sqlite3.SQLITE_CREATE_INDEX, sqlite3.SQLITE_DROP_INDEX, sqlite3.SQLITE_CREATE_TRIGGER, sqlite3.SQLITE_DROP_TRIGGER:
		return Write{Kind: actions[action], Table: a2}, true
	case sqlite3.SQLITE_ALTER_TABLE:
		return Write{Kind: actions[action], Table: a2}, true
	case sqlite3.SQLITE_CREATE_TABLE, sqlite3.SQLITE_DROP_TABLE, sqlite3.SQLITE_CREATE_VIEW,
		sqlite3.SQLITE_DROP_VIEW, sqlite3.SQLITE_CREATE_VTABLE, sqlite3.SQLITE_DROP_VTABLE:
		if !isOwn(a1) {
			return Write{}, false
		}
		return Write{Kind: actions[action], Table: a1}, true
	}
	return Write{}, false
}

var actions = map[int32]string{
	sqlite3.SQLITE_CREATE_INDEX: "create index", sqlite3.SQLITE_CREATE_TABLE: "create table",
	sqlite3.SQLITE_CREATE_TEMP_INDEX: "create temp index", sqlite3.SQLITE_CREATE_TEMP_TABLE: "create temp table",
	sqlite3.SQLITE_CREATE_TEMP_TRIGGER: "create temp trigger", sqlite3.SQLITE_CREATE_TEMP_VIEW: "create temp view",
	sqlite3.SQLITE_CREATE_TRIGGER: "create trigger", sqlite3.SQLITE_CREATE_VIEW: "create view",
	sqlite3.SQLITE_DELETE: "delete from", sqlite3.SQLITE_DROP_INDEX: "drop index",
	sqlite3.SQLITE_DROP_TABLE: "drop table", sqlite3.SQLITE_DROP_TRIGGER: "drop trigger",
	sqlite3.SQLITE_DROP_VIEW: "drop view", sqlite3.SQLITE_INSERT: "insert into",
	sqlite3.SQLITE_PRAGMA: "pragma", sqlite3.SQLITE_READ: "read", sqlite3.SQLITE_TRANSACTION: "transaction",
	sqlite3.SQLITE_UPDATE: "update", sqlite3.SQLITE_ATTACH: "attach", sqlite3.SQLITE_DETACH: "detach",
	sqlite3.SQLITE_ALTER_TABLE: "alter table", sqlite3.SQLITE_REINDEX: "reindex", sqlite3.SQLITE_ANALYZE: "analyze",
	sqlite3.SQLITE_CREATE_VTABLE: "create virtual table", sqlite3.SQLITE_DROP_VTABLE: "drop virtual table",
	sqlite3.SQLITE_FUNCTION: "function", sqlite3.SQLITE_SAVEPOINT: "savepoint",
}

// describe names a refused action for its error, e.g. "attach" or
// "pragma writable_schema". Anything in the temp database is a temp object:
// SQLite's first action creating one is a write to sqlite_temp_master.
func describe(action int32, a1, a2, db string) string {
	if db == "temp" {
		return "creating temp objects"
	}
	name, ok := actions[action]
	if !ok {
		name = "this statement"
	}
	what := a1
	switch action {
	case sqlite3.SQLITE_ALTER_TABLE, sqlite3.SQLITE_FUNCTION:
		what = a2
	case sqlite3.SQLITE_PRAGMA:
		what = strings.TrimSpace(a1 + " " + a2)
	case sqlite3.SQLITE_ATTACH, sqlite3.SQLITE_TRANSACTION, sqlite3.SQLITE_SAVEPOINT:
		what = ""
	}
	return strings.TrimSpace(name + " " + what)
}
