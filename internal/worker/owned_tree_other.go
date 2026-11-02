//go:build !unix

package worker

func stopOwnedTree(pid int) error { return nil }
