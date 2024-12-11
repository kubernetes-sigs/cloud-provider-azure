package publicip

import (
	"errors"
)

var (
	ErrNotFound = errors.New("public IP not found")
)

func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}
