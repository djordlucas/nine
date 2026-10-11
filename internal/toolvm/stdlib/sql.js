// nine:sql — the database: one SQLite database per instance, shared by its
// processes, with a schema you design and change.
//
// Only a process's program reaches it — a live process, or a slice process's
// call — and only with `sql = true` among its capabilities. Conversations read
// it through the sql_query tool.
//
//   import { exec, query } from "nine:sql";
//
//   exec("CREATE TABLE IF NOT EXISTS seen (url TEXT PRIMARY KEY, at TEXT)");
//   exec("INSERT OR IGNORE INTO seen VALUES (?, ?)", [url, new Date().toISOString()]);
//   const { rows } = query("SELECT count(*) FROM seen");   // rows: [[42]]
//
// Allowed: reads, writes and schema changes (CREATE, ALTER, DROP, indexes,
// triggers, fts5 tables) on your own tables. Refused, with the reason: ATTACH,
// transactions, temp objects, and pragmas beyond table introspection. The
// database has a size limit and each statement a time limit.

const I = globalThis[Symbol.for("nine.internal")];

function call(op, sql, args) {
  if (typeof sql !== "string") throw new TypeError(`${op}: sql must be a string`);
  const res = JSON.parse(I.sql(JSON.stringify({ op, sql, args: args ?? [] })));
  if (res.error) {
    const err = new Error(res.error);
    err.code = res.code || "E_SQL";
    throw err;
  }
  return res.result;
}

/**
 * Run a statement that returns rows: { columns, rows, truncated? }, rows as
 * arrays in column order. At most 1000 rows.
 */
export function query(sql, args) {
  return call("query", sql, args);
}

/**
 * Run statements that change the database: { changes, last_insert_rowid }.
 * Several statements separated by semicolons run in order.
 */
export function exec(sql, args) {
  return call("exec", sql, args);
}
