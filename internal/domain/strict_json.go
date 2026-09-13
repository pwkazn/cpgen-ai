package domain

import (
	"errors"
	"reflect"
)

// DecodeStrictJSON applies the same exact-field, Unicode and duplicate-key
// rules used by domain contracts to bounded documents in other packages.
// It decodes structure only; the caller must validate semantic bindings.
func DecodeStrictJSON(data []byte, destination any) error {
	value := reflect.ValueOf(destination)
	if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() {
		return errors.New("strict JSON destination must be a nonnil pointer")
	}
	return strictJSON(string(data), destination)
}
