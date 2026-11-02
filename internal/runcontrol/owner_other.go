//go:build !unix

package runcontrol

import (
	"errors"
	"os"
)

func owned(os.FileInfo) bool       { return false }
func Lease(string) (func(), error) { return nil, errors.New("execution controls require Unix") }

func lease(string, bool) (func(), error) { return nil, errors.New("execution controls require Unix") }
