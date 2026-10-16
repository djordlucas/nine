package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"nine/internal/api/apigen"
	"nine/internal/protocol"
)

// The workspace's files and the database (adr/web-ui.md §10). Both go through
// the daemon, which alone opens them (API-A-1); the store is reachable here by
// no path.

// refusal sorts a daemon error into the answer it gets: a refusal the daemon
// explains is the caller's to fix, anything else is the server's.
type refusal int

const (
	refusedServer refusal = iota
	refusedInvalid
	refusedNotFound
	refusedConflict
)

func classify(err error) (refusal, string) {
	msg := err.Error()
	text, fromDaemon := strings.CutPrefix(msg, "daemon: ")
	switch {
	case !fromDaemon:
		return refusedServer, msg
	case strings.HasPrefix(text, "not found:"), strings.HasPrefix(text, "table not found:"):
		return refusedNotFound, text
	case strings.Contains(text, "changed since it was read"),
		strings.HasPrefix(text, "no workspace is configured"),
		text == "this instance has no database":
		return refusedConflict, text
	}
	return refusedInvalid, text
}

// etag quotes a file version as an entity tag; ifMatch reads one back.
func etag(version string) string { return `"` + version + `"` }

func ifMatch(h *string) string {
	if h == nil {
		return ""
	}
	return strings.Trim(strings.TrimPrefix(strings.TrimSpace(*h), "W/"), `"`)
}

func (s *Server) ListWorkspaceFiles(ctx context.Context, request apigen.ListWorkspaceFilesRequestObject) (apigen.ListWorkspaceFilesResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListWorkspaceFiles503JSONResponse(errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	var out apigen.WorkspaceListing
	args := protocol.WorkspaceArgs{}
	if request.Params.Path != nil {
		args.Path = *request.Params.Path
	}
	if err := cl.Workspace(protocol.WorkspaceList, args, &out); err != nil {
		switch kind, msg := classify(err); kind {
		case refusedNotFound:
			return apigen.ListWorkspaceFiles404JSONResponse(errorBody("not_found", msg, nil)), nil
		case refusedConflict:
			return apigen.ListWorkspaceFiles409JSONResponse(errorBody("conflict", msg, nil)), nil
		case refusedInvalid:
			return apigen.ListWorkspaceFiles400JSONResponse(errorBody("invalid_request", msg, nil)), nil
		default:
			return apigen.ListWorkspaceFiles500JSONResponse(errorBody("server_error", msg, nil)), nil
		}
	}
	return apigen.ListWorkspaceFiles200JSONResponse(out), nil
}

func (s *Server) GetWorkspaceFile(ctx context.Context, request apigen.GetWorkspaceFileRequestObject) (apigen.GetWorkspaceFileResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.GetWorkspaceFile503JSONResponse(errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	var f protocol.WorkspaceFile
	if err := cl.Workspace(protocol.WorkspaceRead, protocol.WorkspaceArgs{Path: request.Params.Path}, &f); err != nil {
		switch kind, msg := classify(err); kind {
		case refusedNotFound:
			return apigen.GetWorkspaceFile404JSONResponse(errorBody("not_found", msg, nil)), nil
		case refusedConflict:
			return apigen.GetWorkspaceFile409JSONResponse(errorBody("conflict", msg, nil)), nil
		case refusedInvalid:
			return apigen.GetWorkspaceFile400JSONResponse(errorBody("invalid_request", msg, nil)), nil
		default:
			return apigen.GetWorkspaceFile500JSONResponse(errorBody("server_error", msg, nil)), nil
		}
	}
	return workspaceFile{file: f, download: request.Params.Download != nil && *request.Params.Download}, nil
}

// workspaceFile answers in place of the generated response, for the headers it
// does not set. A workspace file is whatever an agent or a person put there,
// so it is never served as anything a browser would run on the UI's origin:
// always octet-stream, never sniffed, and sandboxed should it be opened.
type workspaceFile struct {
	file     protocol.WorkspaceFile
	download bool
}

func (r workspaceFile) VisitGetWorkspaceFileResponse(w http.ResponseWriter) error {
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Content-Length", fmt.Sprint(len(r.file.Content)))
	h.Set("ETag", etag(r.file.Version))
	if t, err := time.Parse(time.RFC3339, r.file.Modified); err == nil {
		h.Set("Last-Modified", t.UTC().Format(http.TimeFormat))
	}
	disposition := "inline"
	if r.download {
		disposition = "attachment"
	}
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": path.Base(r.file.Path)}))
	w.WriteHeader(http.StatusOK)
	_, err := io.Copy(w, bytes.NewReader(r.file.Content))
	return err
}

func (s *Server) PutWorkspaceFile(ctx context.Context, request apigen.PutWorkspaceFileRequestObject) (apigen.PutWorkspaceFileResponseObject, error) {
	content, err := io.ReadAll(io.LimitReader(request.Body, protocol.MaxWorkspaceFileBytes+1))
	if err != nil {
		return apigen.PutWorkspaceFile400JSONResponse(errorBody("invalid_request", "read the body: "+err.Error(), nil)), nil
	}
	if len(content) > protocol.MaxWorkspaceFileBytes {
		return apigen.PutWorkspaceFile400JSONResponse(errorBody("invalid_request",
			fmt.Sprintf("the file is over the %d MB this writes", protocol.MaxWorkspaceFileBytes>>20), nil)), nil
	}
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.PutWorkspaceFile503JSONResponse(errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	var out apigen.WorkspaceEntry
	args := protocol.WorkspaceArgs{Path: request.Params.Path, Content: content, IfVersion: ifMatch(request.Params.IfMatch)}
	if err := cl.Workspace(protocol.WorkspaceWrite, args, &out); err != nil {
		switch kind, msg := classify(err); kind {
		case refusedConflict:
			return apigen.PutWorkspaceFile409JSONResponse(errorBody("conflict", msg, nil)), nil
		case refusedInvalid, refusedNotFound:
			return apigen.PutWorkspaceFile400JSONResponse(errorBody("invalid_request", msg, nil)), nil
		default:
			return apigen.PutWorkspaceFile500JSONResponse(errorBody("server_error", msg, nil)), nil
		}
	}
	return apigen.PutWorkspaceFile200JSONResponse(out), nil
}

func (s *Server) DeleteWorkspaceFile(ctx context.Context, request apigen.DeleteWorkspaceFileRequestObject) (apigen.DeleteWorkspaceFileResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.DeleteWorkspaceFile503JSONResponse(errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	var out apigen.WorkspaceEntry
	args := protocol.WorkspaceArgs{Path: request.Params.Path, IfVersion: ifMatch(request.Params.IfMatch)}
	if err := cl.Workspace(protocol.WorkspaceDelete, args, &out); err != nil {
		switch kind, msg := classify(err); kind {
		case refusedNotFound:
			return apigen.DeleteWorkspaceFile404JSONResponse(errorBody("not_found", msg, nil)), nil
		case refusedConflict:
			return apigen.DeleteWorkspaceFile409JSONResponse(errorBody("conflict", msg, nil)), nil
		case refusedInvalid:
			return apigen.DeleteWorkspaceFile400JSONResponse(errorBody("invalid_request", msg, nil)), nil
		default:
			return apigen.DeleteWorkspaceFile500JSONResponse(errorBody("server_error", msg, nil)), nil
		}
	}
	return apigen.DeleteWorkspaceFile200JSONResponse(out), nil
}

func (s *Server) ListDatabaseTables(ctx context.Context, request apigen.ListDatabaseTablesRequestObject) (apigen.ListDatabaseTablesResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListDatabaseTables503JSONResponse(errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	var out apigen.ListDatabaseTablesResponse
	if err := cl.Database(protocol.DatabaseTables, protocol.DatabaseArgs{}, &out); err != nil {
		if kind, msg := classify(err); kind == refusedConflict {
			return apigen.ListDatabaseTables409JSONResponse(errorBody("conflict", msg, nil)), nil
		} else {
			return apigen.ListDatabaseTables500JSONResponse(errorBody("server_error", msg, nil)), nil
		}
	}
	return apigen.ListDatabaseTables200JSONResponse(out), nil
}

func (s *Server) ListDatabaseRows(ctx context.Context, request apigen.ListDatabaseRowsRequestObject) (apigen.ListDatabaseRowsResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListDatabaseRows503JSONResponse(errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	args := protocol.DatabaseArgs{Table: request.Table}
	if request.Params.Offset != nil {
		args.Offset = *request.Params.Offset
	}
	if request.Params.Limit != nil {
		args.Limit = *request.Params.Limit
	}
	var out apigen.DatabaseRows
	if err := cl.Database(protocol.DatabaseRows, args, &out); err != nil {
		switch kind, msg := classify(err); kind {
		case refusedNotFound:
			return apigen.ListDatabaseRows404JSONResponse(errorBody("not_found", msg, nil)), nil
		case refusedConflict:
			return apigen.ListDatabaseRows409JSONResponse(errorBody("conflict", msg, nil)), nil
		default:
			return apigen.ListDatabaseRows500JSONResponse(errorBody("server_error", msg, nil)), nil
		}
	}
	return apigen.ListDatabaseRows200JSONResponse(out), nil
}

// databaseWrite runs a row change or a statement and sorts its failure.
func (s *Server) databaseWrite(action string, args protocol.DatabaseArgs) (apigen.DatabaseRunResult, refusal, string) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.DatabaseRunResult{}, unavailable, err.Error()
	}
	defer cl.Close()
	var out apigen.DatabaseRunResult
	if err := cl.Database(action, args, &out); err != nil {
		kind, msg := classify(err)
		return out, kind, msg
	}
	return out, 0, ""
}

// unavailable marks a daemon that could not be reached, for databaseWrite.
const unavailable refusal = -1

func (s *Server) InsertDatabaseRow(ctx context.Context, request apigen.InsertDatabaseRowRequestObject) (apigen.InsertDatabaseRowResponseObject, error) {
	out, kind, msg := s.databaseWrite(protocol.DatabaseInsert, protocol.DatabaseArgs{Table: request.Table, Values: request.Body.Values})
	switch {
	case msg == "":
		return apigen.InsertDatabaseRow200JSONResponse(out), nil
	case kind == unavailable:
		return apigen.InsertDatabaseRow503JSONResponse(errorBody("service_unavailable", msg, nil)), nil
	case kind == refusedNotFound:
		return apigen.InsertDatabaseRow404JSONResponse(errorBody("not_found", msg, nil)), nil
	case kind == refusedConflict:
		return apigen.InsertDatabaseRow409JSONResponse(errorBody("conflict", msg, nil)), nil
	case kind == refusedInvalid:
		return apigen.InsertDatabaseRow400JSONResponse(errorBody("invalid_request", msg, nil)), nil
	}
	return apigen.InsertDatabaseRow500JSONResponse(errorBody("server_error", msg, nil)), nil
}

func (s *Server) UpdateDatabaseRow(ctx context.Context, request apigen.UpdateDatabaseRowRequestObject) (apigen.UpdateDatabaseRowResponseObject, error) {
	out, kind, msg := s.databaseWrite(protocol.DatabaseUpdate,
		protocol.DatabaseArgs{Table: request.Table, RowID: request.Rowid, Values: request.Body.Values})
	switch {
	case msg == "":
		return apigen.UpdateDatabaseRow200JSONResponse(out), nil
	case kind == unavailable:
		return apigen.UpdateDatabaseRow503JSONResponse(errorBody("service_unavailable", msg, nil)), nil
	case kind == refusedNotFound:
		return apigen.UpdateDatabaseRow404JSONResponse(errorBody("not_found", msg, nil)), nil
	case kind == refusedConflict:
		return apigen.UpdateDatabaseRow409JSONResponse(errorBody("conflict", msg, nil)), nil
	case kind == refusedInvalid:
		return apigen.UpdateDatabaseRow400JSONResponse(errorBody("invalid_request", msg, nil)), nil
	}
	return apigen.UpdateDatabaseRow500JSONResponse(errorBody("server_error", msg, nil)), nil
}

func (s *Server) DeleteDatabaseRow(ctx context.Context, request apigen.DeleteDatabaseRowRequestObject) (apigen.DeleteDatabaseRowResponseObject, error) {
	out, kind, msg := s.databaseWrite(protocol.DatabaseDelete, protocol.DatabaseArgs{Table: request.Table, RowID: request.Rowid})
	switch {
	case msg == "":
		return apigen.DeleteDatabaseRow200JSONResponse(out), nil
	case kind == unavailable:
		return apigen.DeleteDatabaseRow503JSONResponse(errorBody("service_unavailable", msg, nil)), nil
	case kind == refusedNotFound:
		return apigen.DeleteDatabaseRow404JSONResponse(errorBody("not_found", msg, nil)), nil
	case kind == refusedConflict:
		return apigen.DeleteDatabaseRow409JSONResponse(errorBody("conflict", msg, nil)), nil
	case kind == refusedInvalid:
		return apigen.DeleteDatabaseRow400JSONResponse(errorBody("invalid_request", msg, nil)), nil
	}
	return apigen.DeleteDatabaseRow500JSONResponse(errorBody("server_error", msg, nil)), nil
}

func (s *Server) RunDatabaseSQL(ctx context.Context, request apigen.RunDatabaseSQLRequestObject) (apigen.RunDatabaseSQLResponseObject, error) {
	args := protocol.DatabaseArgs{SQL: request.Body.Sql}
	if request.Body.Args != nil {
		args.Args = *request.Body.Args
	}
	out, kind, msg := s.databaseWrite(protocol.DatabaseRun, args)
	switch {
	case msg == "":
		return apigen.RunDatabaseSQL200JSONResponse(out), nil
	case kind == unavailable:
		return apigen.RunDatabaseSQL503JSONResponse(errorBody("service_unavailable", msg, nil)), nil
	case kind == refusedConflict:
		return apigen.RunDatabaseSQL409JSONResponse(errorBody("conflict", msg, nil)), nil
	case kind == refusedInvalid, kind == refusedNotFound:
		return apigen.RunDatabaseSQL400JSONResponse(errorBody("invalid_request", msg, nil)), nil
	}
	return apigen.RunDatabaseSQL500JSONResponse(errorBody("server_error", msg, nil)), nil
}
