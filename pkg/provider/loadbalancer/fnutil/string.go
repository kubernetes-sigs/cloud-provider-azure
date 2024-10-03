package fnutil

type Stringer interface {
	String() string
}

func AsString[T Stringer](v T) string {
	return v.String()
}
