package uniparse

import (
	"encoding/json"

	"gopkg.in/yaml.v3"
)

func Unmarshal(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		if err := yaml.Unmarshal(data, v); err != nil {
			return err
		}
	}
	return nil
}
