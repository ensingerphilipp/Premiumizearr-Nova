package config

func (c *Config) UpdateConfig(_newConfig Config) {
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

	c.appCallback(oldConfig, *c)
	c.Save()
}
