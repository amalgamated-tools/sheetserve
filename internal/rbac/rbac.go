// Package rbac says what each user may do on the server, by role.
package rbac

import "slices"

type Role string

const (
	Admin   Role = "admin"   // everything a member can, and manages users
	Member  Role = "member"  // manages their own spreadsheets and keys, whose API calls work
	Pending Role = "pending" // signed in, waiting for an admin to approve them
)

// Roles lists every role, most powerful first.
var Roles = []Role{Admin, Member, Pending}

type Permission string

const (
	EditOwn     Permission = "edit_own"     // add spreadsheets, set tab rules, make keys
	UseAPI      Permission = "use_api"      // the user's keys reach the REST API and MCP
	ManageUsers Permission = "manage_users" // approve, change the role of, or remove users
)

var grants = map[Role][]Permission{
	Admin:  {EditOwn, UseAPI, ManageUsers},
	Member: {EditOwn, UseAPI},
}

// Can reports whether r grants p. An unknown role grants nothing.
func (r Role) Can(p Permission) bool { return slices.Contains(grants[r], p) }

func (r Role) Valid() bool { return slices.Contains(Roles, r) }
