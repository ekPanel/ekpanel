package api

import (
	"net/http"

	"ekpanel/internal/catalog"
	"ekpanel/internal/store"
)

type catalogEntry struct {
	catalog.Template
	// Allowed is false when the user's package does not include all of the template's images.
	Allowed bool `json:"allowed"`
}

// handleCatalog lists the one-click services. Customers see whether their package allows each.
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	of, err := s.offerFor(r)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	list := []catalogEntry{}
	for _, t := range catalog.All() {
		allowed := true
		for _, image := range t.Images {
			if of != nil && !imagesAllow(of.AppImages, image) {
				allowed = false
			}
		}
		list = append(list, catalogEntry{Template: t, Allowed: allowed})
	}
	writeJSON(w, http.StatusOK, list)
}

func templateEnv(vars []catalog.Variable) []store.EnvVar {
	env := make([]store.EnvVar, len(vars))
	for i, v := range vars {
		env[i] = store.EnvVar{Key: v.Key, Value: v.Value}
	}
	return env
}
