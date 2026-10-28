package web

import (
	"net/http"
	"strings"

	"github.com/amalgamated-tools/sheetserve/internal/db"
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
	key := db.NewKey()
	err := d.DB.CreateKey(r.Context(), u.id, db.Key{
		Hash: db.HashKey(key), Name: strings.TrimSpace(r.FormValue("name")), Tabs: r.Form["tabs"], Ops: r.Form["ops"],
	})
	if err != nil {
		d.render(w, r, u, view{path: keysPage, err: userMessage(err)}, http.StatusBadRequest)
		return
	}
	d.Tenants.Invalidate(u.id)
	d.render(w, r, u, view{path: keysPage, newKey: key}, http.StatusOK)
}

func (d *Dashboard) deleteKey(w http.ResponseWriter, r *http.Request, u user) {
	d.change(w, r, u, keysPage, d.DB.DeleteKey(r.Context(), u.id, r.FormValue("name")))
}
