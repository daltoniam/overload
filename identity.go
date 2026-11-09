package overload

import "time"

// Roles a signed-in person can have. Admins manage models, workflows,
// repositories and users; members also request reviews and run schedules;
// viewers only read.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
	RoleViewer = "viewer"
)

var roleRanks = map[string]int{RoleViewer: 1, RoleMember: 2, RoleAdmin: 3}

// RoleAtLeast reports whether role grants at least minimum.
func RoleAtLeast(role, minimum string) bool {
	return roleRanks[role] > 0 && roleRanks[role] >= roleRanks[minimum]
}

// ValidRole reports whether role is a known role.
func ValidRole(role string) bool { return roleRanks[role] > 0 }

// User is a person who signed in through the OIDC provider. Their role is
// set from the provider's groups at every sign-in.
type User struct {
	ID          int64
	Issuer      string
	Subject     string
	Email       string
	Name        string
	Role        string
	Disabled    bool
	LastLoginAt *time.Time
}
