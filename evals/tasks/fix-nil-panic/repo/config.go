package fixture

// Config holds runtime settings. Timeout is optional; nil means unset.
type Config struct {
	Timeout *int
}

// EffectiveTimeout returns the configured timeout, or 30 when unset.
func EffectiveTimeout(c *Config) int {
	return *c.Timeout
}
