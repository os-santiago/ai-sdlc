package repositorytests

import "golang.org/x/sys/unix"

func unixMkfifo(path string) error { return unix.Mkfifo(path, 0o600) }
