package httpapi

import (
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/domain"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store"
)

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	switch r.Method {
	case http.MethodGet:
		sessions, err := s.service.Store().ListSessions(r.Context())
		if err != nil {
			writeStoreResult(w, http.StatusOK, nil, err)
			return
		}
		writeJSON(w, http.StatusOK, listResponse[domain.Session]{Items: sessions})
	case http.MethodPost:
		var input struct {
			SnapshotID   string `json:"snapshot_id"`
			Type         string `json:"type"`
			Title        string `json:"title"`
			Description  string `json:"description"`
			ArtifactName string `json:"artifact_name"`
			ArtifactPath string `json:"artifact_path"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if input.SnapshotID == "" {
			input.SnapshotID = uuid.NewString()
		}
		session, err := s.service.CreateSession(r.Context(), store.SessionCreate{SnapshotID: input.SnapshotID, Type: input.Type, Title: input.Title, Description: input.Description, ArtifactName: input.ArtifactName, ArtifactPath: input.ArtifactPath})
		writeStoreResult(w, http.StatusCreated, session, err)
	default:
		writeMethodNotAllowed(w)
	}
}

func (s *Server) handleSessionUpload(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	defer file.Close()

	artifactPath, err := s.storeArtifact(file, header)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, artifactUploadResponse{ArtifactName: header.Filename, ArtifactPath: artifactPath})
}

func (s *Server) handleSessionSubresources(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	parts := splitPath(r.URL.Path)
	if len(parts) < 4 {
		writeNotFound(w)
		return
	}
	snapshotID := parts[3]
	if len(parts) == 5 && parts[4] == "download" {
		s.handleSessionDownload(w, r, snapshotID)
		return
	}
	switch r.Method {
	case http.MethodGet:
		session, err := s.service.Store().GetSession(r.Context(), snapshotID)
		writeStoreResult(w, http.StatusOK, session, err)
	case http.MethodPatch:
		var input struct {
			Title        *string `json:"title"`
			Description  *string `json:"description"`
			ArtifactName *string `json:"artifact_name"`
			ArtifactPath *string `json:"artifact_path"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		session, err := s.service.UpdateSession(r.Context(), snapshotID, store.SessionUpdate{Title: input.Title, Description: input.Description, ArtifactName: input.ArtifactName, ArtifactPath: input.ArtifactPath})
		writeStoreResult(w, http.StatusOK, session, err)
	case http.MethodDelete:
		writeDeleteResult(w, s.service.DeleteSession(r.Context(), snapshotID))
	default:
		writeMethodNotAllowed(w)
	}
}

func (s *Server) handleSessionDownload(w http.ResponseWriter, r *http.Request, snapshotID string) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	session, err := s.service.Store().GetSession(r.Context(), snapshotID)
	if err != nil {
		writeStoreResult(w, http.StatusOK, nil, err)
		return
	}
	if strings.HasPrefix(session.ArtifactPath, "s3://") {
		writeError(w, http.StatusNotImplemented, "not_implemented", "s3 artifact download redirect is not implemented yet")
		return
	}
	http.ServeFile(w, r, session.ArtifactPath)
}

func (s *Server) storeArtifact(file multipart.File, header *multipart.FileHeader) (string, error) {
	if err := os.MkdirAll(s.artifactsDir, 0o755); err != nil {
		return "", err
	}
	name := uuid.NewString() + "-" + filepath.Base(header.Filename)
	path := filepath.Join(s.artifactsDir, name)
	out, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := out.ReadFrom(file); err != nil {
		return "", err
	}
	return path, nil
}
