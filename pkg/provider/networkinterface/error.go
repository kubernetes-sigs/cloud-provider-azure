package networkinterface

import (
	"errors"
)

var (
	ErrNotFound = errors.New("network interface not found")
)

func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}
