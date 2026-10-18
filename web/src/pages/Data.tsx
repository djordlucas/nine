import { useState } from 'preact/hooks';
import { useLocation, useRoute } from 'preact-iso';
import { api, errorMessage, get, type Schemas } from '../api/client';
import { useLive } from '../live';
import { asText, cell, crumbs, dirHref, fieldValue, fileHref, join, MAX_EDIT_BYTES, parent, version } from '../data';
import { bytes } from '../system';
import { Toggle } from '../components/Toggle';
import { Area } from '../components/Area';
import { ask } from '../components/Confirm';
import { ListHead, Split } from '../components/Split';
import split from '../components/Split.module.css';
import detail from '../components/Detail.module.css';
import { param } from './Processes';
import tools from './Tools.module.css';
import styles from './Data.module.css';
import { Icon } from '../components/Icon';

type Entry = Schemas['WorkspaceEntry'];
type Table = Schemas['DatabaseTable'];
type RunResult = Schemas['DatabaseRunResult'];

const views = [
  { path: '/data/files', label: 'Files' },
  { path: '/data/database', label: 'Database' },
  { path: '/data/sql', label: 'SQL' },
];

/**
 * Data (adr/web-ui.md §10): the workspace's files and the database processes
 * keep their records in, to read and change. Nine's own state, the store and
 * the workspace's .nine/, is not reachable from here.
 */
export function Data() {
  const { path, query } = useLocation();
  const { params } = useRoute();
  const view = views.find((v) => path === v.path || path.startsWith(v.path + '/'))?.path ?? '/data/files';
  return (
    <div class={tools.section}>
      <nav class={tools.views} aria-label="Data views">
        {views.map((v) => (
          <a key={v.path} href={v.path} class={tools.view} aria-current={v.path === view ? 'page' : undefined}>
            {v.label}
          </a>
        ))}
      </nav>
      {view === '/data/files' && <Files file={query.file} dir={query.file !== undefined ? parent(query.file) : (query.dir ?? '')} />}
      {view === '/data/database' && <Database table={param(params.table as string | undefined)} />}
      {view === '/data/sql' && <Sql />}
    </div>
  );
}

// Files

function Files({ dir, file }: { dir: string; file?: string }) {
  const { route } = useLocation();
  const [changed, setChanged] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const reload = () => setChanged((n) => n + 1);
  const { data, error: loadError } = useLive(
    async () => get(api.GET('/workspace/files', { params: { query: { path: dir } } }), 'the directory'),
    [],
    `${dir}#${changed}`,
  );

  const put = async (path: string, body: Blob | string, ifVersion?: string) => {
    const { data: e, error: err } = await api.PUT('/workspace/file', {
      params: { query: { path }, header: ifVersion ? { 'If-Match': `"${ifVersion}"` } : {} },
      body: body as unknown as string,
      bodySerializer: (b: unknown) => b as BodyInit,
      headers: { 'Content-Type': 'application/octet-stream' },
    });
    if (!e) throw new Error(errorMessage(err, `Could not write ${path}.`));
    return e;
  };

  const newFile = async () => {
    const name = prompt('New file name (a path such as notes/today.md makes its directories):');
    if (!name?.trim()) return;
    const path = join(dir, name);
    if (data?.entries.some((e) => e.path === path)) {
      setError(`${path} already exists.`);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      await put(path, '');
      reload();
      route(fileHref(path));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const upload = async (files: FileList | null) => {
    if (!files?.length) return;
    const existing = new Set(data?.entries.map((e) => e.name));
    const replaced = [...files].filter((f) => existing.has(f.name)).map((f) => f.name);
    if (replaced.length && !(await ask({ title: `Replace ${replaced.join(', ')}?`, message: 'The current version goes to the trash.', confirm: 'Replace' }))) return;
    setBusy(true);
    setError(null);
    try {
      for (const f of files) await put(join(dir, f.name), f);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
      reload();
    }
  };

  return (
    <Split name="files" noun="files" open={file !== undefined}>
      <nav class={split.list} aria-label="Files">
        <ListHead>
          <ol class={styles.crumbs} aria-label="Directory">
            {crumbs(dir).map((c) => (
              <li key={c.path}>
                <a href={dirHref(c.path)} aria-current={c.path === dir ? 'page' : undefined}>
                  {c.name}
                </a>
              </li>
            ))}
          </ol>
        </ListHead>
        <div class={detail.actions}>
          <button type="button" class={`${detail.action} ${detail.iconAction}`} disabled={busy} onClick={() => void newFile()} aria-label="New file" title="New file">
            <Icon name="filePlus" size={18} />
          </button>
          <label class={`${detail.action} ${detail.iconAction} ${styles.upload}`} title="Upload">
            <Icon name="upload" size={18} />
            <input
              type="file"
              aria-label="Upload"
              multiple
              disabled={busy}
              onChange={(e) => {
                const input = e.currentTarget;
                void upload(input.files).then(() => (input.value = ''));
              }}
            />
          </label>
        </div>
        {(error || loadError) && <p class="error">{error || loadError}</p>}
        {!data && !loadError && <p class="muted">Loading…</p>}
        {data && data.entries.length === 0 && <p class="muted">This directory is empty.</p>}
        {data && data.entries.length > 0 && (
          <ul>
            {data.entries.map((e) => (
              <li key={e.path}>
                <a
                  href={e.dir ? dirHref(e.path) : fileHref(e.path)}
                  class={split.row}
                  aria-current={e.path === file ? 'page' : undefined}
                >
                  <span class={detail.badge}>{e.dir ? 'dir' : 'file'}</span>
                  <span class={split.name}>{e.name}</span>
                  <span class={split.meta}>
                    {e.dir ? new Date(e.modified).toLocaleString() : `${bytes(e.bytes)} · ${new Date(e.modified).toLocaleString()}`}
                  </span>
                </a>
              </li>
            ))}
          </ul>
        )}
        {data && dir !== '' && data.entries.length === 0 && (
          <button type="button" class={`${detail.action} ${detail.iconAction} ${detail.danger}`} disabled={busy} onClick={() => void removeDir(dir)} aria-label="Delete this directory" title="Delete this directory">
            <Icon name="trash" size={18} />
          </button>
        )}
      </nav>
      <section class={split.pane} aria-label="File">
        {file !== undefined ? (
          <FileDetail key={`${file}#${changed}`} path={file} put={put} onChanged={reload} />
        ) : (
          <p class={`card muted ${split.placeholder}`}>
            The workspace: the files Nine's tools read and write, and yours. Choose a file to read or edit it.
          </p>
        )}
      </section>
    </Split>
  );

  async function removeDir(path: string) {
    setBusy(true);
    const { data: e, error: err } = await api.DELETE('/workspace/file', { params: { query: { path } } });
    setBusy(false);
    if (!e) {
      setError(errorMessage(err, `Could not delete ${path}.`));
      return;
    }
    route(dirHref(parent(path)));
  }
}

interface Loaded {
  text: string | null;
  size: number;
  version?: string;
  modified: string | null;
}

function FileDetail(props: {
  path: string;
  put: (path: string, body: string, ifVersion?: string) => Promise<Entry>;
  onChanged: () => void;
}) {
  const { route } = useLocation();
  const { path } = props;
  const [draft, setDraft] = useState<string | null>(null);
  const [saved, setSaved] = useState<Loaded | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const { data: loaded, error: loadError } = useLive(async (): Promise<Loaded | 'missing'> => {
    const r = await api.GET('/workspace/file', { params: { query: { path } }, parseAs: 'arrayBuffer' });
    if (r.response.status === 404) return 'missing';
    if (r.data === undefined) throw new Error(errorMessage(r.error, `Could not read ${path}.`));
    const buf = r.data as unknown as ArrayBuffer;
    return {
      text: asText(buf),
      size: buf.byteLength,
      version: version(r.response.headers.get('ETag')),
      modified: r.response.headers.get('Last-Modified'),
    };
  }, []);
  const file = saved ?? loaded;
  const name = path.split('/').pop();
  const download = `/api/v1/workspace/file?path=${encodeURIComponent(path)}&download=true`;

  if (loadError) return <p class="card error">{loadError}</p>;
  if (file === 'missing') return <p class="card muted">No file {path}.</p>;
  if (file === null) return <p class="card muted">Loading…</p>;

  const dirty = draft !== null && draft !== file.text;
  const save = async () => {
    if (draft === null) return;
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      const e = await props.put(path, draft, file.version);
      setSaved({ text: draft, size: e.bytes, version: e.version, modified: e.modified });
      setDraft(null);
      setNotice(e.trashed ? 'Saved. The previous version is in the trash.' : 'Saved.');
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  const remove = async () => {
    if (!(await ask({ title: `Delete ${path}?`, message: "It goes to the trash, where the agent's restore_file can bring it back.", confirm: 'Delete', danger: true }))) return;
    setBusy(true);
    const { data: e, error: err } = await api.DELETE('/workspace/file', {
      params: { query: { path }, header: file.version ? { 'If-Match': `"${file.version}"` } : {} },
    });
    setBusy(false);
    if (!e) {
      setError(errorMessage(err, `Could not delete ${path}.`));
      return;
    }
    props.onChanged();
    route(dirHref(parent(path)));
  };

  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <a class={detail.back} href={dirHref(parent(path))} aria-label="The directory">
          ‹
        </a>
        <div class={detail.title}>
          <h2>{name}</h2>
          <span class="muted">
            {[path, bytes(file.size), file.modified ? new Date(file.modified).toLocaleString() : '']
              .filter(Boolean)
              .join(' · ')}
          </span>
        </div>
        <div class={detail.actions}>
          {file.text !== null && (
            <button type="button" class={`${detail.action} ${detail.primary}`} disabled={busy || !dirty} onClick={() => void save()}>
              Save
            </button>
          )}
          <a class={`${detail.action} ${detail.iconAction} ${styles.linkButton}`} href={download} download={name} aria-label="Download" title="Download">
            <Icon name="download" size={18} />
          </a>
          <button type="button" class={`${detail.action} ${detail.iconAction} ${detail.danger}`} disabled={busy} onClick={() => void remove()} aria-label="Delete" title="Delete this file">
            <Icon name="trash" size={18} />
          </button>
        </div>
      </header>
      {error && (
        <p class="error" role="alert">
          {error}
          {error.includes('changed since') && ' Copy your changes, then reload the file to see the new version.'}
        </p>
      )}
      {notice && <p class={detail.notice}>{notice}</p>}
      {file.text !== null ? (
        <Area
          class={styles.editor}
          aria-label={`Contents of ${name}`}
          spellcheck={false}
          value={draft ?? file.text}
          onInput={(e) => setDraft(e.currentTarget.value)}
          onKeyDown={(e) => {
            if ((e.metaKey || e.ctrlKey) && e.key === 's') {
              e.preventDefault();
              if (dirty && !busy) void save();
            }
          }}
        />
      ) : (
        <p class="muted">
          {file.size > MAX_EDIT_BYTES ? `Over ${bytes(MAX_EDIT_BYTES)}, too large to edit here.` : 'Not a text file.'} Download it
          to open it, or upload a new version in its directory.
        </p>
      )}
    </article>
  );
}

// Database

function tableHref(name: string): string {
  return `/data/database/${encodeURIComponent(name)}`;
}

function Database({ table }: { table?: string }) {
  const [changed, setChanged] = useState(0);
  const { data, error } = useLive(
    async () => (await get(api.GET('/database/tables'), 'the tables')).tables,
    ['sql_write'],
    changed,
  );
  return (
    <Split name="tables" noun="tables" open={!!table}>
      <nav class={split.list} aria-label="Tables">
        <ListHead>
          <p class={split.heading}>
            <span>Tables and views</span>
            {data && <span>{data.length}</span>}
          </p>
        </ListHead>
        {error && <p class="error">{error}</p>}
        {!data && !error && <p class="muted">Loading…</p>}
        {data && data.length === 0 && <p class="muted">No tables yet. Processes create them; so can the SQL view.</p>}
        {data && data.length > 0 && (
          <ul>
            {data.map((t) => (
              <li key={t.name}>
                <a href={tableHref(t.name)} class={split.row} aria-current={t.name === table ? 'page' : undefined}>
                  <span class={detail.badge}>{t.kind}</span>
                  <span class={split.name}>{t.name}</span>
                  <span class={split.meta}>
                    {t.rows} rows · {t.columns.length} columns
                  </span>
                </a>
              </li>
            ))}
          </ul>
        )}
      </nav>
      <section class={split.pane} aria-label="Table">
        {table ? (
          data ? (
            <TableDetail key={table} table={data.find((t) => t.name === table)} name={table} onChanged={() => setChanged((n) => n + 1)} />
          ) : (
            <p class="card muted">Loading…</p>
          )
        ) : (
          <p class={`card muted ${split.placeholder}`}>
            The database processes keep their records in. Choose a table to read and edit its rows.
          </p>
        )}
      </section>
    </Split>
  );
}

const PAGE = 100;

/** The row form's target: a row by its rowid, or a new row. */
type Editing = { rowid: number; values: unknown[] } | { rowid: null } | null;

function TableDetail({ table, name, onChanged }: { table?: Table; name: string; onChanged: () => void }) {
  const [offset, setOffset] = useState(0);
  const [editing, setEditing] = useState<Editing>(null);
  const [changed, setChanged] = useState(0);
  const { data, error } = useLive(
    async () =>
      get(
        api.GET('/database/tables/{table}/rows', { params: { path: { table: name }, query: { offset, limit: PAGE } } }),
        'the rows',
      ),
    ['sql_write'],
    `${offset}#${changed}`,
  );
  if (!table) return <p class="card muted">No table {name}.</p>;
  const editable = table.rowid;
  const done = () => {
    setEditing(null);
    setChanged((n) => n + 1);
    onChanged();
  };
  return (
    <article class={`card ${detail.detail}`}>
      <header class={detail.head}>
        <a class={detail.back} href="/data/database" aria-label="All tables">
          ‹
        </a>
        <div class={detail.title}>
          <h2>{table.name}</h2>
          <span class="muted">
            {table.kind} · {table.rows} rows
          </span>
        </div>
        {table.kind === 'table' && (
          <div class={detail.actions}>
            <button type="button" class={`${detail.action} ${detail.iconAction} ${detail.primary}`} onClick={() => setEditing({ rowid: null })} aria-label="Add row" title="Add row">
              <Icon name="plus" size={18} />
            </button>
          </div>
        )}
      </header>
      {!editable && (
        <p class="muted">
          {table.kind === 'view' ? 'A view shows rows other tables hold.' : 'This table has no rowid, so its rows are changed with a statement.'} Use
          the <a href="/data/sql">SQL</a> view to change it.
        </p>
      )}
      {editing && <RowForm key={editing.rowid ?? 'new'} table={table} editing={editing} onDone={done} onCancel={() => setEditing(null)} />}
      {error && <p class="error">{error}</p>}
      {!data && !error && <p class="muted">Loading…</p>}
      {data && (
        <>
          <Grid
            columns={data.columns}
            rows={data.rows}
            label={`Rows of ${table.name}`}
            onRow={
              editable && data.rowids
                ? (i) => setEditing({ rowid: data.rowids![i], values: data.rows[i] })
                : undefined
            }
          />
          <div class={styles.pager}>
            <button type="button" class={`${detail.action} ${detail.iconAction}`} disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE))} aria-label="Previous" title="Previous page">
              <Icon name="prev" size={18} />
            </button>
            <span class="muted">
              {data.total === 0 ? 'No rows' : `${offset + 1}–${offset + data.rows.length} of ${data.total}`}
            </span>
            <button type="button" class={`${detail.action} ${detail.iconAction}`} disabled={offset + PAGE >= data.total} onClick={() => setOffset(offset + PAGE)} aria-label="Next" title="Next page">
              <Icon name="next" size={18} />
            </button>
          </div>
        </>
      )}
    </article>
  );
}

function RowForm(props: { table: Table; editing: NonNullable<Editing>; onDone: () => void; onCancel: () => void }) {
  const { table, editing } = props;
  const existing = editing.rowid !== null ? (editing as { values: unknown[] }).values : null;
  const initial = table.columns.map((_, i) => (existing ? existing[i] : undefined));
  const [texts, setTexts] = useState(initial.map((v) => (v === null || v === undefined ? '' : cell(v))));
  const [nulls, setNulls] = useState(initial.map((v) => existing !== null && v === null));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const values = () => {
    const out: Record<string, unknown> = {};
    table.columns.forEach((c, i) => {
      const touched = existing ? texts[i] !== (initial[i] === null ? '' : cell(initial[i])) || nulls[i] !== (initial[i] === null) : texts[i] !== '' || nulls[i];
      if (touched) out[c.name] = fieldValue(texts[i], nulls[i], c.type);
    });
    return out;
  };
  const submit = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    const body = { values: values() };
    const r =
      editing.rowid === null
        ? await api.POST('/database/tables/{table}/rows', { params: { path: { table: table.name } }, body })
        : Object.keys(body.values).length === 0
          ? { data: {} as RunResult, error: undefined }
          : await api.PATCH('/database/tables/{table}/rows/{rowid}', {
              params: { path: { table: table.name, rowid: editing.rowid } },
              body,
            });
    setBusy(false);
    if (!r.data) {
      setError(errorMessage(r.error, 'The change was refused.'));
      return;
    }
    props.onDone();
  };
  const remove = async () => {
    if (editing.rowid === null || !(await ask({ title: `Delete row ${editing.rowid} of ${table.name}?`, message: 'This cannot be undone.', confirm: 'Delete', danger: true }))) return;
    setBusy(true);
    const { data, error: err } = await api.DELETE('/database/tables/{table}/rows/{rowid}', {
      params: { path: { table: table.name, rowid: editing.rowid } },
    });
    setBusy(false);
    if (!data) {
      setError(errorMessage(err, 'Could not delete the row.'));
      return;
    }
    props.onDone();
  };

  return (
    <form class={styles.form} onSubmit={(e) => void submit(e)} aria-label={editing.rowid === null ? 'New row' : `Row ${editing.rowid}`}>
      <h3>{editing.rowid === null ? 'New row' : `Row ${editing.rowid}`}</h3>
      {table.columns.map((c, i) => (
        <div key={c.name} class={styles.field}>
          <label for={`col-${i}`}>
            {c.name} <span class="muted">{[c.type, c.pk ? 'key' : '', c.not_null ? 'required' : ''].filter(Boolean).join(' · ')}</span>
          </label>
          <Area
            id={`col-${i}`}
            class="input"
            rows={1}
            max={200}
            disabled={nulls[i]}
            placeholder={editing.rowid === null ? (c.default ? `default ${c.default}` : c.pk ? 'automatic' : '') : ''}
            value={texts[i]}
            onInput={(e) => setTexts(texts.map((t, j) => (j === i ? e.currentTarget.value : t)))}
          />
          <Toggle on={nulls[i]} onChange={(on) => setNulls(nulls.map((n, j) => (j === i ? on : n)))} title={`Store NULL in ${c.name}`}>
            NULL
          </Toggle>
        </div>
      ))}
      {error && (
        <p class="error" role="alert">
          {error}
        </p>
      )}
      <div class={detail.actions}>
        <button type="submit" class={`${detail.action} ${detail.primary}`} disabled={busy}>
          {editing.rowid === null ? 'Insert' : 'Save'}
        </button>
        <button type="button" class={`${detail.action} ${detail.secondary}`} disabled={busy} onClick={props.onCancel}>
          Cancel
        </button>
        {editing.rowid !== null && (
          <button type="button" class={`${detail.action} ${detail.iconAction} ${detail.danger}`} disabled={busy} onClick={() => void remove()} aria-label="Delete row" title="Delete row">
            <Icon name="trash" size={18} />
          </button>
        )}
      </div>
    </form>
  );
}

function Grid(props: { columns: string[]; rows: unknown[][]; label: string; onRow?: (i: number) => void }) {
  if (props.rows.length === 0) return null;
  return (
    <div class={styles.gridWrap}>
      <table class={styles.grid} aria-label={props.label}>
        <thead>
          <tr>
            {props.columns.map((c, i) => (
              <th key={i} scope="col">
                {c}
              </th>
            ))}
            {props.onRow && <th scope="col" aria-label="Edit" />}
          </tr>
        </thead>
        <tbody>
          {props.rows.map((r, i) => (
            <tr key={i}>
              {r.map((v, j) => (
                <td key={j} class={v === null ? styles.null : undefined} title={cell(v)}>
                  {cell(v)}
                </td>
              ))}
              {props.onRow && (
                <td>
                  <button type="button" class={styles.edit} onClick={() => props.onRow!(i)}>
                    Edit
                  </button>
                </td>
              )}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// SQL

function Sql() {
  const [sql, setSql] = useState('');
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<RunResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const run = async (e: Event) => {
    e.preventDefault();
    if (!sql.trim()) return;
    setBusy(true);
    setError(null);
    const { data, error: err } = await api.POST('/database/sql', { body: { sql } });
    setBusy(false);
    setResult(data ?? null);
    if (!data) setError(errorMessage(err, 'The statement was refused.'));
  };
  return (
    <section class={`card ${detail.detail}`} aria-labelledby="sql-h">
      <h2 id="sql-h" class={styles.title}>
        Run a statement
      </h2>
      <p class="muted">
        One statement on the database. A read shows its rows; a write runs under the rules a process's program follows: no
        ATTACH, transactions, temporary objects or pragmas beyond table introspection.
      </p>
      <form class={styles.sql} onSubmit={(e) => void run(e)}>
        <Area
          class={styles.editor}
          max={400}
          aria-label="Statement"
          spellcheck={false}
          placeholder="SELECT * FROM …"
          value={sql}
          onInput={(e) => setSql(e.currentTarget.value)}
          onKeyDown={(e) => {
            if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') void run(e);
          }}
        />
        <div class={detail.actions}>
          <button type="submit" class={`${detail.action} ${detail.primary}`} disabled={busy || !sql.trim()}>
            Run
          </button>
        </div>
      </form>
      {error && (
        <p class="error" role="alert">
          {error}
        </p>
      )}
      {result && result.write && (
        <p class={detail.notice} role="status">
          {result.changes} {result.changes === 1 ? 'row' : 'rows'} changed
          {result.last_insert_rowid ? `; last inserted rowid ${result.last_insert_rowid}` : ''}.
        </p>
      )}
      {result && !result.write && (
        <>
          <p class="muted" role="status">
            {result.rows.length} {result.rows.length === 1 ? 'row' : 'rows'}
            {result.truncated ? ', the first 1000 shown' : ''}.
          </p>
          <Grid columns={result.columns} rows={result.rows} label="Result" />
        </>
      )}
    </section>
  );
}
