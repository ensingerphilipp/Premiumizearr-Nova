package config

// PersistUpdate replaces the config with _newConfig and persists it,
// WITHOUT firing the app fan-out. The replace and Save() run first so a
// failed save can roll the in-memory config back to its previous value:
// the running state must never diverge from the on-disk file (e.g. the
// DirectClientAPIKey the compat endpoints authenticate with), and an
// unpersisted change must not restart or re-key live services. Callers
// that must deliver a response before triggering the fan-out (the web
// config route: the fan-out's in-handler web-server restart closes the
// very connection serving the request) call NotifyApp themselves once
// their response is on the wire.
func (c *Config) PersistUpdate(_newConfig Config) error {
	oldConfig := *c

	// An update body that omits Arrs decodes as a nil slice, which the API
	// would then serve back as "Arrs": null; normalize to an empty slice so
	// the array field always serializes as an array.
	if _newConfig.Arrs == nil {
		_newConfig.Arrs = []ArrConfig{}
	}

	//move private fields over
	_newConfig.appCallback = c.appCallback
	_newConfig.altConfigLocation = c.altConfigLocation
	*c = _newConfig
	if err := c.Save(); err != nil {
		// Persistence failed: restore the previous config in memory so
		// the running state matches the on-disk file, and surface the
		// error instead of fanning out a change that never reached disk.
		*c = oldConfig
		return err
	}
	return nil
}

// NotifyApp fires the app fan-out for an already-persisted update: the
// web server's in-handler restart on a BindIP/BindPort/WebRoot/
// DirectClientAPIKey change, and the other services' config swaps.
func (c *Config) NotifyApp(oldConfig, newConfig Config) {
	c.appCallback(oldConfig, newConfig)
}

// UpdateConfig persists a new config and, only after the save succeeded,
// fires the app fan-out, returning the save error otherwise. Callers
// that must interleave their own response between persistence and the
// fan-out (the web config route, whose update's in-handler web-server
// restart would close the connection serving the request before the
// response was written) use PersistUpdate and NotifyApp instead.
func (c *Config) UpdateConfig(_newConfig Config) error {
	oldConfig := *c
	if err := c.PersistUpdate(_newConfig); err != nil {
		return err
	}
	c.appCallback(oldConfig, *c)
	return nil
}
