package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"nine/internal/api/apigen"
	"nine/internal/backup"
	"nine/internal/protocol"
)

// The CLI-only operations the web UI needs (adr/web-ui.md §4): reflections,
// the whole session roster, and backup. Each goes through the daemon, which
// alone opens the store (API-A-1).

// selfReflectionAgent is the daemon's own reflecting session, the default of
// `nine reflections`.
const selfReflectionAgent = "self-reflection"

func (s *Server) ListReflections(ctx context.Context, request apigen.ListReflectionsRequestObject) (apigen.ListReflectionsResponseObject, error) {
	agent := selfReflectionAgent
	if request.Params.Agent != nil && *request.Params.Agent != "" {
		agent = *request.Params.Agent
	}
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListReflections503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	events, err := cl.SessionEvents(agent, 0)
	if err != nil {
		// A session with no journal has reflected nothing yet.
		if errNotFound(err) {
			return apigen.ListReflections200JSONResponse{Agent: agent, Data: []apigen.Reflection{}}, nil
		}
		return apigen.ListReflections500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	return apigen.ListReflections200JSONResponse{Agent: agent, Data: reflectionsOf(events)}, nil
}

// reflectionsOf keeps what each turn concluded: a turn_end with a result and no
// error, as `nine reflections` prints them.
func reflectionsOf(events []protocol.JournalEvent) []apigen.Reflection {
	out := []apigen.Reflection{}
	for _, e := range events {
		if e.Type != "turn_end" {
			continue
		}
		var p struct {
			Result string `json:"result"`
			Error  string `json:"error"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || p.Error != "" || p.Result == "" {
			continue
		}
		out = append(out, apigen.Reflection{At: e.TS, Turn: ptr(e.Turn), Text: p.Result})
	}
	return out
}

func toSessionInfo(si protocol.SessionInfo) apigen.SessionInfo {
	out := apigen.SessionInfo{Id: si.ID, Status: si.Status, AgeSeconds: si.AgeSeconds}
	if si.Name != "" {
		out.Name = ptr(si.Name)
	}
	out.Events = ptr(si.Events)
	out.Protected = ptr(si.Protected)
	out.Attached = ptr(si.Attached)
	return out
}

func (s *Server) ListSessions(ctx context.Context, request apigen.ListSessionsRequestObject) (apigen.ListSessionsResponseObject, error) {
	p, err := page(request.Params.Limit, request.Params.Offset)
	if err != nil {
		return apigen.ListSessions400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListSessions503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	sessions, err := cl.ListSessions()
	if err != nil {
		return apigen.ListSessions500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	infos := make([]apigen.SessionInfo, 0, len(sessions))
	for _, si := range sessions {
		infos = append(infos, toSessionInfo(si))
	}
	data, pagination := paginate(infos, p)
	return apigen.ListSessions200JSONResponse{Data: &data, Pagination: &pagination}, nil
}

// findSession returns the session with exactly id. The daemon resolves a
// prefix, which suits a person typing at a terminal; over HTTP, and before a
// delete above all, only the exact id may match.
func findSession(cl *protocol.Client, id string) (protocol.SessionInfo, bool, error) {
	sessions, err := cl.ListSessions()
	if err != nil {
		return protocol.SessionInfo{}, false, err
	}
	for _, si := range sessions {
		if si.ID == id {
			return si, true, nil
		}
	}
	return protocol.SessionInfo{}, false, nil
}

func (s *Server) GetSession(ctx context.Context, request apigen.GetSessionRequestObject) (apigen.GetSessionResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.GetSession503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	si, ok, err := findSession(cl, request.Id)
	switch {
	case err != nil:
		return apigen.GetSession500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	case !ok:
		return apigen.GetSession404JSONResponse(
			errorBody("not_found", "session not found", map[string]any{"id": request.Id})), nil
	}
	return apigen.GetSession200JSONResponse(toSessionInfo(si)), nil
}

func (s *Server) DeleteSession(ctx context.Context, request apigen.DeleteSessionRequestObject) (apigen.DeleteSessionResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.DeleteSession503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	_, ok, err := findSession(cl, request.Id)
	switch {
	case err != nil:
		return apigen.DeleteSession500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	case !ok:
		return apigen.DeleteSession404JSONResponse(
			errorBody("not_found", "session not found", map[string]any{"id": request.Id})), nil
	}
	msg, err := cl.DeleteSession(request.Id)
	if err != nil {
		return apigen.DeleteSession500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	return apigen.DeleteSession200JSONResponse{Id: request.Id, Message: msg}, nil
}

func (s *Server) GetBackup(ctx context.Context, request apigen.GetBackupRequestObject) (apigen.GetBackupResponseObject, error) {
	withWorkspace := request.Params.With != nil && *request.Params.With == apigen.Workspace
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.GetBackup503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	res, err := cl.Backup(withWorkspace)
	if err != nil {
		if withWorkspace && strings.Contains(err.Error(), "no workspace") {
			return apigen.GetBackup400JSONResponse(
				errorBody("invalid_request", err.Error(), nil)), nil
		}
		return apigen.GetBackup500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	f, err := os.Open(res.Path)
	if err != nil {
		backup.RemoveTemp(res.Path) //nolint:errcheck
		return apigen.GetBackup500JSONResponse(
			errorBody("server_error", fmt.Sprintf("open the snapshot: %v", err), nil)), nil
	}
	return backupDownload{file: f, res: res}, nil
}

// backupDownload streams the daemon's snapshot and removes it afterwards. It
// answers in place of the generated response so it can lift the server's
// write timeout first: a workspace archive can take longer to send than any
// request is allowed to.
type backupDownload struct {
	file *os.File
	res  protocol.BackupResult
}

func (b backupDownload) VisitGetBackupResponse(w http.ResponseWriter) error {
	defer func() {
		b.file.Close() //nolint:errcheck
		if err := backup.RemoveTemp(b.res.Path); err != nil {
			slog.Warn("backup: remove the snapshot", "path", b.res.Path, "err", err)
		}
	}()
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		slog.Debug("backup: lift write deadline", "err", err)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(b.res.Bytes))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", b.res.Name))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, err := io.Copy(w, b.file)
	return err
}
