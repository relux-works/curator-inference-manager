package infra

// Windows has no supported Unix socket broker.
func ValidateEngineCacheRoot(string) error { return nil }
