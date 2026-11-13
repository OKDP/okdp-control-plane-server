package models

// Kinds of project members: a user or a group, as named in the OIDC token.
const (
	MemberKindUser  = "user"
	MemberKindGroup = "group"
)

// Roles on a project, from the least to the most privileged.
const (
	ProjectRoleView  = "view"
	ProjectRoleEdit  = "edit"
	ProjectRoleAdmin = "admin"
)

// ProjectRoles lists the roles from the least to the most privileged.
var ProjectRoles = []string{ProjectRoleView, ProjectRoleEdit, ProjectRoleAdmin}

// ProjectMember is a user or a group holding a role on a project.
type ProjectMember struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// ProjectMembers is the member list of a project, and whether the caller may
// change it.
type ProjectMembers struct {
	Members   []ProjectMember `json:"members"`
	CanManage bool            `json:"canManage"`
}
