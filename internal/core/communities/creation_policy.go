package communities

// CreationPolicy says who may create a community on this instance. It is set
// by the COMMUNITY_CREATION environment variable; the create handler treats
// any value other than CreationAnyone as CreationInstanceAdmins.
type CreationPolicy string

const (
	// CreationAnyone lets any signed-in account create a community.
	CreationAnyone CreationPolicy = "anyone"
	// CreationInstanceAdmins restricts community creation to instance admins.
	CreationInstanceAdmins CreationPolicy = "instance-admins"
)
