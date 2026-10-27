package rbac

import "testing"

func TestCan(t *testing.T) {
	for _, tc := range []struct {
		role Role
		perm Permission
		want bool
	}{
		{Admin, ManageUsers, true},
		{Admin, UseAPI, true},
		{Member, EditOwn, true},
		{Member, ManageUsers, false},
		{Pending, EditOwn, false},
		{Pending, UseAPI, false},
		{"", UseAPI, false},
	} {
		if got := tc.role.Can(tc.perm); got != tc.want {
			t.Errorf("%q.Can(%q) = %v", tc.role, tc.perm, got)
		}
	}
	if Role("owner").Valid() || !Pending.Valid() {
		t.Error("Valid")
	}
}
