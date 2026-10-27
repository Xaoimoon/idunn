// Package privdrop lets Iðunn start as root and keep only what it needs: an
// unprivileged user plus the group that owns the Docker socket. The socket
// group differs between hosts, so it is read at startup instead of being
// configured.
package privdrop

import (
	"fmt"
	"os"
	"syscall"
)

// Drop switches a root process to uid:uid with the socket's group as a
// supplementary group. It does nothing when the process is not root (the
// user was set explicitly). It fails rather than go on as root.
func Drop(socket string, uid int) (string, error) {
	if os.Getuid() != 0 {
		return "", nil
	}
	groups := []int{}
	var st syscall.Stat_t
	if socket != "" {
		if err := syscall.Stat(socket, &st); err == nil {
			groups = append(groups, int(st.Gid))
		}
	}
	if err := syscall.Setgroups(groups); err != nil {
		return "", fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(uid); err != nil {
		return "", fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return "", fmt.Errorf("setuid: %w", err)
	}
	// Getting root back must be impossible.
	if syscall.Setuid(0) == nil || os.Getuid() == 0 {
		return "", fmt.Errorf("still able to regain root after dropping privileges")
	}
	return fmt.Sprintf("uid=%d gid=%d groups=%v", os.Getuid(), os.Getgid(), groups), nil
}
