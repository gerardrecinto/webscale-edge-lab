//go:build !linux

package hostdoctor

func filesystems(string) []FSUsage { return nil }
