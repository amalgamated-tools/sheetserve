package web

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
	"github.com/amalgamated-tools/sheetserve/internal/rbac"
)

// users lists everyone who has signed in, marking viewerID as the viewer.
func (d *Dashboard) users(ctx context.Context, viewerID int64) ([]userView, error) {
	users, err := d.DB.Users(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]userView, len(users))
	for i, v := range users {
		views[i] = userView{v.ID, v.Email, v.Role, v.ID == viewerID}
	}
	return views, nil
}

// setRole changes another user's role, such as approving a pending one.
func (d *Dashboard) setRole(w http.ResponseWriter, r *http.Request, u user) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	role := rbac.Role(r.FormValue("role"))
	if err == nil {
		err = d.DB.SetRole(r.Context(), id, role)
	}
	if err == nil {
		audit(r.Context(), u, "user.set_role", slog.Int64(otelkeys.TargetUserID, id), slog.String(otelkeys.Role, string(role)))
	}
	d.change(w, r, u, usersPage, err)
}

// deleteUser removes a user and everything they added. Their keys stop working at once.
func (d *Dashboard) deleteUser(w http.ResponseWriter, r *http.Request, u user) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err == nil {
		err = d.DB.DeleteUser(r.Context(), id)
	}
	if err == nil {
		d.Tenants.Invalidate(id)
		audit(r.Context(), u, "user.delete", slog.Int64(otelkeys.TargetUserID, id))
	}
	d.change(w, r, u, usersPage, err)
}
