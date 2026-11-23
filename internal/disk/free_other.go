//go:build !windows && !linux && !darwin

package disk

import "errors"

func free(string) (uint64, error) { return 0, errors.New("free space isn't known on this system") }
