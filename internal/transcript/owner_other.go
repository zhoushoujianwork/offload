//go:build !unix

package transcript

import "os"

func ownedByUs(os.FileInfo) bool { return true }

// otherAccess is not checked where permission bits do not describe access.
func otherAccess(os.FileInfo) bool { return false }
