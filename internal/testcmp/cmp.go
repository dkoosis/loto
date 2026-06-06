package testcmp

import (
	"fmt"
	"reflect"
)

// Diff returns an empty string when want and got are deeply equal. It is a
// small local test helper used in environments where external test assertion
// modules are unavailable.
func Diff(want, got any) string {
	if reflect.DeepEqual(want, got) {
		return ""
	}
	return fmt.Sprintf("want: %#v\ngot:  %#v", want, got)
}
