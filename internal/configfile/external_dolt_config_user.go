package configfile

// ResolvedUser returns the configured database user or the default root user.
func (c ExternalDoltConfig) ResolvedUser() string {
	if c.User == "" {
		return ExternalDoltConfigDefaultUser
	}
	return c.User
}
