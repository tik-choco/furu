package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/tik-choco/furu/internal/atomicfile"
)

func ParseTarget(name string) (string, error) {
	switch name {
	case "orchestrator", "o":
		return "orchestrator", nil
	case "simple", "s":
		return "simple", nil
	case "normal", "n":
		return "normal", nil
	case "hard", "h":
		return "hard", nil
	case "very_hard", "vh":
		return "very_hard", nil
	default:
		return "", fmt.Errorf("unknown model target %q", name)
	}
}

func ParseBackend(target, name string) (string, error) {
	switch name {
	case "claude", "cl":
		return "claude", nil
	case "codex", "cx":
		if target == "orchestrator" {
			return "", errors.New("orchestrator only supports claude")
		}
		return "codex", nil
	default:
		return "", fmt.Errorf("unknown model backend %q", name)
	}
}

// SetModel stores only the nonempty fields in change.
func SetModel(path, target, backend string, change Model) (Config, error) {
	target, err := ParseTarget(target)
	if err != nil {
		return Config{}, err
	}
	backend, err = ParseBackend(target, backend)
	if err != nil {
		return Config{}, err
	}
	set := func(entry object) error {
		for _, field := range []struct{ name, value string }{
			{"model", change.Model},
			{"effort", change.Effort},
		} {
			if field.value != "" {
				value, _ := json.Marshal(field.value)
				entry[entry.field(field.name)] = value
			}
		}
		return nil
	}
	return editFile(path, func(doc object) error {
		if target == "orchestrator" {
			return doc.edit(doc.field(target), set)
		}
		return doc.edit(doc.field("tiers"), func(tiers object) error {
			return tiers.edit(target, func(tier object) error {
				return tier.edit(tier.field(backend), set)
			})
		})
	})
}

// ResetModels removes overrides so future changes to the defaults apply too.
func ResetModels(path, target string) (Config, error) {
	if target != "" {
		var err error
		target, err = ParseTarget(target)
		if err != nil {
			return Config{}, err
		}
	}
	return editFile(path, func(doc object) error {
		if target == "" || target == "orchestrator" {
			doc.deleteField("orchestrator")
		}
		if target != "orchestrator" {
			return doc.edit(doc.field("tiers"), func(tiers object) error {
				if target != "" {
					delete(tiers, target)
				} else {
					for _, name := range TierNames() {
						delete(tiers, name)
					}
				}
				return nil
			})
		}
		return nil
	})
}

type object map[string]json.RawMessage

// Struct field names are case-insensitive when loading JSON.
func (o object) field(name string) string {
	if _, ok := o[name]; !ok {
		for key := range o {
			if strings.EqualFold(key, name) {
				return key
			}
		}
	}
	return name
}

func (o object) deleteField(name string) {
	for key := range o {
		if strings.EqualFold(key, name) {
			delete(o, key)
		}
	}
}

func (o object) edit(key string, mutate func(object) error) error {
	var child object
	if raw, ok := o[key]; ok {
		if err := json.Unmarshal(raw, &child); err != nil {
			return err
		}
	}
	if child == nil {
		child = make(object)
	}
	wasEmpty := len(child) == 0
	if err := mutate(child); err != nil {
		return err
	}
	if len(child) == 0 {
		if !wasEmpty {
			delete(o, key)
		}
		return nil
	}
	value, err := json.Marshal(child)
	if err == nil {
		o[key] = value
	}
	return err
}

func editFile(path string, mutate func(object) error) (Config, error) {
	doc := make(object)
	data, err := os.ReadFile(path)
	if err == nil {
		if _, err := decode(path, data); err != nil {
			return Config{}, err
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			return Config{}, fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, err
	}
	if err := mutate(doc); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	data, err = json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return Config{}, err
	}
	cfg, err := decode(path, data)
	if err != nil {
		return Config{}, err
	}
	if err := atomicfile.Write(path, append(data, '\n')); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
