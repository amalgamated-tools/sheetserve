package web

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
)

func (d *Dashboard) createKey(w http.ResponseWriter, r *http.Request, u user) {
	if err := r.ParseForm(); err != nil {
		d.render(w, r, u, view{path: keysPage, err: "could not read the form"}, http.StatusBadRequest)
		return
	}
	if len(r.Form["tabs"]) == 0 {
		d.render(w, r, u, view{path: keysPage, err: "pick at least one tab for the key"}, http.StatusBadRequest)
		return
	}
	key, name := db.NewKey(), strings.TrimSpace(r.FormValue("name"))
	err := d.DB.CreateKey(r.Context(), u.id, db.Key{
		Hash: db.HashKey(key), Name: name, Tabs: r.Form["tabs"], Ops: r.Form["ops"],
	})
	if err != nil {
		d.render(w, r, u, view{path: keysPage, err: userMessage(err)}, http.StatusBadRequest)
		return
	}
	d.Tenants.Invalidate(u.id)
	audit(r.Context(), u, "key.create", slog.String(otelkeys.KeyName, name),
		slog.String(otelkeys.Tabs, strings.Join(r.Form["tabs"], ",")), slog.String(otelkeys.Ops, strings.Join(r.Form["ops"], ",")))
	d.render(w, r, u, view{path: keysPage, newKey: key}, http.StatusOK)
}

func (d *Dashboard) deleteKey(w http.ResponseWriter, r *http.Request, u user) {
	name := r.FormValue("name")
	err := d.DB.DeleteKey(r.Context(), u.id, name)
	if err == nil {
		audit(r.Context(), u, "key.delete", slog.String(otelkeys.KeyName, name))
	}
	d.change(w, r, u, keysPage, err)
}
