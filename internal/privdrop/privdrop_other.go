//go:build !linux

package privdrop

// Drop is a no-op outside Linux, where Iðunn only runs for development.
func Drop(socket string, uid int) (string, error) { return "", nil }
