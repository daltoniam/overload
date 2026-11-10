package overload

import "testing"

func TestRoleAtLeast(t *testing.T) {
	for _, test := range []struct {
		role, minimum string
		want          bool
	}{
		{RoleAdmin, RoleMember, true},
		{RoleMember, RoleMember, true},
		{RoleViewer, RoleMember, false},
		{RoleViewer, RoleViewer, true},
		{"", RoleViewer, false},
		{"owner", RoleViewer, false},
		{RoleAdmin, "admn", false},
		{RoleViewer, "", false},
	} {
		if got := RoleAtLeast(test.role, test.minimum); got != test.want {
			t.Errorf("RoleAtLeast(%q, %q) = %v", test.role, test.minimum, got)
		}
	}
}
