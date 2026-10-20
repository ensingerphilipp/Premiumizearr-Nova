package config

func (c *Config) UpdateConfig(_newConfig Config) {
	oldConfig := *c

	// An update body that omits Arrs decodes as a nil slice, which the API
	// would then serve back as "Arrs": null; normalize to an empty slice so
	// the array field always serializes as an array.
	if _newConfig.Arrs == nil {
		_newConfig.Arrs = []ArrConfig{}
	}

	// Older API clients omit the transfer cap. Preserve a configured value,
	// and fall back to the default if neither config has a positive value.
	if _newConfig.MaxActivePremiumizeTransfers <= 0 {
		_newConfig.MaxActivePremiumizeTransfers = c.MaxActivePremiumizeTransfers
		if _newConfig.MaxActivePremiumizeTransfers <= 0 {
			_newConfig.MaxActivePremiumizeTransfers = DefaultMaxActivePremiumizeTransfers
		}
	}

	//move private fields over
	_newConfig.appCallback = c.appCallback
	_newConfig.altConfigLocation = c.altConfigLocation
	*c = _newConfig

	c.appCallback(oldConfig, *c)
	c.Save()
}
