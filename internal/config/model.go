package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SetModel updates one profile's model without rewriting its provider or secrets.
func SetModel(path, name, model string) error {
	if name == "" {
		return fmt.Errorf("profile name must not be empty")
	}
	c, err := Load(path)
	if err != nil {
		return err
	}
	p, ok := c.Profiles[name]
	if !ok {
		return fmt.Errorf("unknown profile %q", name)
	}
	p.Model = strings.TrimSpace(model)
	c.Profiles[name] = p
	if _, _, err := c.Resolve(name); err != nil {
		return err
	}
	fields, err := modeSettings(path)
	if err != nil {
		return err
	}
	var profiles map[string]json.RawMessage
	if err := json.Unmarshal(fields["profiles"], &profiles); err != nil {
		return err
	}
	var profile map[string]json.RawMessage
	if err := json.Unmarshal(profiles[name], &profile); err != nil {
		return err
	}
	if profile == nil {
		return fmt.Errorf("profile %q must be a JSON object", name)
	}
	profile["model"], _ = json.Marshal(p.Model)
	profiles[name], err = json.Marshal(profile)
	if err != nil {
		return err
	}
	fields["profiles"], err = json.Marshal(profiles)
	if err != nil {
		return err
	}
	return writeSettings(path, fields)
}
