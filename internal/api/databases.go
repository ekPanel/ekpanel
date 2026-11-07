package api

import (
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"ekpanel/internal/auth"
	"ekpanel/internal/store"
)

var dbNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// reservedDBNames are system databases and accounts on MariaDB and PostgreSQL, and the
// panel's own database. The agent refuses them too.
var reservedDBNames = []string{"root", "admin", "mysql", "mariadb", "information_schema", "performance_schema", "sys",
	"postgres", "template0", "template1", "ekpanel", "public", "pg_signal_backend", "debian_sys_maint", "phpmyadmin"}

func validDBName(n string) bool {
	return dbNameRE.MatchString(n) && !slices.Contains(reservedDBNames, n) && !strings.HasPrefix(n, "pg_")
}

func passwordHashFor(engine, password string) string {
	if engine == "mariadb" {
		return auth.MariaDBNativeHash(password)
	}
	return auth.PostgresSCRAMVerifier(password)
}

// databaseWithPassword is returned when a password is created; it is shown only this once.
type databaseWithPassword struct {
	Database store.Database `json:"database"`
	Password string         `json:"password"`
}

type createDatabaseRequest struct {
	ServerID string `json:"server_id"`
	Engine   string `json:"engine"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

func (s *Server) writeDatabaseStoreError(w http.ResponseWriter, r *http.Request, err error) {
	var taken store.ErrDatabaseNameTaken
	switch {
	case errors.As(err, &taken):
		code := "database_name_taken"
		if taken.Field == "user name" {
			code = "database_user_taken"
		}
		writeError(w, http.StatusConflict, code, taken.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "database_not_found", "no database with that ID")
	default:
		s.internalError(w, r, err)
	}
}

func (s *Server) handleListDatabases(w http.ResponseWriter, r *http.Request) {
	serverFilter := r.URL.Query().Get("server_id")
	if serverFilter != "" && !isUUID(serverFilter) {
		writeJSON(w, http.StatusOK, []store.Database{})
		return
	}
	list, err := s.store.ListDatabases(r.Context(), serverFilter)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateDatabase(w http.ResponseWriter, r *http.Request) {
	var req createDatabaseRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.ToLower(strings.TrimSpace(req.Name))
	req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	if req.Username == "" {
		req.Username = req.Name
	}
	if req.Engine != "mariadb" && req.Engine != "postgresql" {
		writeError(w, http.StatusBadRequest, "invalid_engine", "choose MariaDB or PostgreSQL")
		return
	}
	if !validDBName(req.Name) {
		writeError(w, http.StatusBadRequest, "invalid_database_name", "use 1–32 lowercase letters, digits and _ , starting with a letter")
		return
	}
	if !validDBName(req.Username) {
		writeError(w, http.StatusBadRequest, "invalid_database_user", "use 1–32 lowercase letters, digits and _ , starting with a letter")
		return
	}
	if !isUUID(req.ServerID) {
		writeError(w, http.StatusBadRequest, "server_not_found", "choose a server")
		return
	}
	srv, err := s.store.GetServer(r.Context(), req.ServerID)
	if s.serverLookupFailed(w, r, err) {
		return
	}
	password := auth.NewDatabasePassword()
	u := currentUser(r)
	d, err := s.store.CreateDatabase(r.Context(), store.NewDatabase{
		ServerID: srv.ID, Engine: req.Engine, Name: req.Name, Username: req.Username,
		PasswordHash: passwordHashFor(req.Engine, password), CreatedBy: &u.ID,
	})
	if err != nil {
		s.writeDatabaseStoreError(w, r, err)
		return
	}
	s.audit(r, "database.created", d.Name, map[string]any{"database_id": d.ID, "engine": d.Engine, "server_id": srv.ID})
	s.queueReconcile(r, srv.ID)
	writeJSON(w, http.StatusCreated, databaseWithPassword{Database: d, Password: password})
}

func databaseID(r *http.Request) string {
	if id := chi.URLParam(r, "id"); isUUID(id) {
		return id
	}
	return nilUUID
}

func (s *Server) handleGetDatabase(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.GetDatabase(r.Context(), databaseID(r))
	if err != nil {
		s.writeDatabaseStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handleResetDatabasePassword creates a new password; the old one stops working once the
// agent applies it.
func (s *Server) handleResetDatabasePassword(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.GetDatabase(r.Context(), databaseID(r))
	if err != nil {
		s.writeDatabaseStoreError(w, r, err)
		return
	}
	if d.State == "deleting" {
		writeError(w, http.StatusConflict, "database_deleting", "this database is being deleted")
		return
	}
	password := auth.NewDatabasePassword()
	d, err = s.store.SetDatabasePassword(r.Context(), d.ID, passwordHashFor(d.Engine, password))
	if err != nil {
		s.writeDatabaseStoreError(w, r, err)
		return
	}
	s.audit(r, "database.password_reset", d.Name, map[string]any{"database_id": d.ID})
	s.queueReconcile(r, d.ServerID)
	writeJSON(w, http.StatusOK, databaseWithPassword{Database: d, Password: password})
}

func (s *Server) handleDeleteDatabase(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.GetDatabase(r.Context(), databaseID(r))
	if err != nil {
		s.writeDatabaseStoreError(w, r, err)
		return
	}
	d, err = s.store.MarkDatabaseDeleting(r.Context(), d.ID)
	if err != nil {
		s.writeDatabaseStoreError(w, r, err)
		return
	}
	s.audit(r, "database.deleted", d.Name, map[string]any{"database_id": d.ID})
	s.queueReconcile(r, d.ServerID)
	writeJSON(w, http.StatusAccepted, d)
}
