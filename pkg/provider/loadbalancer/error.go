package loadbalancer

import (
	"errors"
)

var ErrNotFound = errors.New("loadbalancer not found")
var ErrBackendPoolNotFound = errors.New("backend pool not found")

func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}

func IsBackendPoolNotFound(err error) bool {
	return errors.Is(err, ErrBackendPoolNotFound)
}
